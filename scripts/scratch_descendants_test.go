package scripts

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func scratchAwaitFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 0 {
			return strings.TrimSpace(string(data))
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
	return ""
}

func scratchStopGroup(t *testing.T, group int) {
	t.Helper()
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		if err := syscall.Kill(-group, sig); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return
			}
			t.Error(err)
		}
		// A stopped fixture must be able to run its pending TERM handler.
		if sig == syscall.SIGTERM {
			if err := syscall.Kill(-group, syscall.SIGCONT); err != nil && !errors.Is(err, syscall.ESRCH) {
				t.Error(err)
			}
		}
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if errors.Is(syscall.Kill(-group, 0), syscall.ESRCH) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	t.Errorf("process group %d survived fixture cleanup", group)
}

type scratchCommand struct {
	done   chan struct{}
	output bytes.Buffer
	err    error
	stop   func()
}

func startScratchCommand(t *testing.T, cmd *exec.Cmd, markers string) *scratchCommand {
	t.Helper()
	run := &scratchCommand{done: make(chan struct{})}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout, cmd.Stderr = &run.output, &run.output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { run.err = cmd.Wait(); close(run.done) }()
	run.stop = func() {
		// A failing regression must still join every fixture process.
		if data, err := os.ReadFile(filepath.Join(markers, "group")); err == nil {
			group, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Error(err)
			} else if group != cmd.Process.Pid {
				scratchStopGroup(t, group)
			}
		}
		// Once its command group is gone, let the supervisor finish its own
		// cleanup before signalling the wrapper group (which may contain rm).
		select {
		case <-run.done:
			return
		case <-time.After(5 * time.Second):
		}
		// Wait concurrently so the direct child's zombie is reaped.
		scratchStopGroup(t, cmd.Process.Pid)
		select {
		case <-run.done:
		case <-time.After(5 * time.Second):
			t.Error("wrapper wait survived cleanup")
		}
	}
	t.Cleanup(run.stop)
	return run
}

func (r *scratchCommand) wait(t *testing.T, status int) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(20 * time.Second):
		t.Fatal("wrapper did not finish bounded shutdown")
	}
	var exit *exec.ExitError
	if status == 0 && r.err != nil || status != 0 && (!errors.As(r.err, &exit) || exit.ExitCode() != status) {
		t.Fatalf("status %d: %v: %s", status, r.err, r.output.String())
	}
}

func TestBuildScratchJoinsDescendants(t *testing.T) {
	root, script := buildScratchFixture(t)
	for _, status := range []int{0, 7, 129, 130, 131, 143} {
		for _, groupSignal := range []bool{false, true} {
			if status < 128 && groupSignal {
				continue
			}
			t.Run(strconv.Itoa(status)+"/group="+strconv.FormatBool(groupSignal), func(t *testing.T) {
				t.Parallel()
				markers := t.TempDir()
				cmd := exec.Command("bash", script, "bash", "-c", `set -eu
ps -o pgid= -p "$$" > "$1/group"
printf '%s\n' "$TMPDIR" > "$1/scratch"
trap 'exit 143' TERM
bash -c '
set -eu
finish() {
    sleep 0.15
    if test -d "$TMPDIR"; then
        printf retained > "$TMPDIR/last-consumer"
        printf retained > "$1/finished"
    else
        printf removed-too-early > "$1/finished"
    fi
    exit 0
}
trap "finish \"\$@\"" TERM
printf ready > "$1/ready"
if test "$2" -lt 128; then finish "$@"; fi
while :; do sleep 0.1; done
' descendant "$1" "$2" &
while ! test -s "$1/ready"; do sleep 0.01; done
if test "$2" -lt 128; then exit "$2"; fi
wait
`, "leader", markers, strconv.Itoa(status))
				run := startScratchCommand(t, cmd, markers)
				scratchAwaitFile(t, filepath.Join(markers, "ready"))
				if status > 128 {
					pid := cmd.Process.Pid
					if groupSignal {
						pid = -pid
					}
					if err := syscall.Kill(pid, syscall.Signal(status-128)); err != nil {
						t.Fatal(err)
					}
				}
				run.wait(t, status)
				if got := scratchAwaitFile(t, filepath.Join(markers, "finished")); got != "retained" {
					t.Fatalf("last descendant observed %s", got)
				}
				scratch := scratchAwaitFile(t, filepath.Join(markers, "scratch"))
				if filepath.Dir(scratch) != filepath.Join(root, ".tmp") {
					t.Fatalf("scratch escaped project: %s", scratch)
				}
				if _, err := os.Stat(scratch); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("scratch survived confirmed shutdown: %v", err)
				}
			})
		}
	}
}
