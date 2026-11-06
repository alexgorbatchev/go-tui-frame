package scripts

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestBuildScratchTerminal(t *testing.T) {
	for _, mode := range []string{"input", "failure", "interrupt", "terminate"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			_, script := buildScratchFixture(t)
			markers := t.TempDir()
			// The outer shell keeps the terminal alive after the wrapper exits,
			// so another read verifies foreground ownership was restored.
			cmd := exec.Command("bash", "-c", `stty tostop
bash "$1" bash -c '
set -eu
ps -o pgid= -p "$$" > "$1/group"
printf "%s\n" "$PPID" > "$1/wrapper"
printf "%s\n" "$TMPDIR" > "$1/scratch"
finish() { printf retained > "$TMPDIR/last-use"; printf finished > "$1/finished"; exit 143; }
trap "finish \"\$@\"" TERM
printf "terminal ready\n"
printf ready > "$1/ready"
IFS= read -r answer
printf "%s\n" "$answer" > "$1/input"
if test "$2" = failure; then exit 7; fi
if test "$2" != input; then while :; do sleep 0.1; done; fi
' probe "$2" "$3"
printf '%s\n' "$?" > "$2/status"
printf restored > "$2/restored"
IFS= read -r answer
printf '%s\n' "$answer" > "$2/after"
`, "terminal", script, markers, mode)
			master, err := pty.Start(cmd)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			var waitErr error
			go func() { waitErr = cmd.Wait(); close(done) }()
			drained := make(chan struct{})
			go func() { _, _ = io.Copy(io.Discard, master); close(drained) }()
			t.Cleanup(func() {
				if data, err := os.ReadFile(filepath.Join(markers, "group")); err == nil {
					group, err := strconv.Atoi(strings.TrimSpace(string(data)))
					if err != nil {
						t.Error(err)
					} else {
						scratchStopGroup(t, group)
					}
				}
				scratchStopGroup(t, cmd.Process.Pid)
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("terminal process wait survived cleanup")
				}
				_ = master.Close()
				select {
				case <-drained:
				case <-time.After(3 * time.Second):
					t.Error("terminal reader survived cleanup")
				}
			})
			scratchAwaitFile(t, filepath.Join(markers, "ready"))
			if mode != "interrupt" {
				if _, err := master.WriteString("build input\n"); err != nil {
					t.Fatal(err)
				}
				if got := scratchAwaitFile(t, filepath.Join(markers, "input")); got != "build input" {
					t.Fatalf("terminal input = %q", got)
				}
			}
			want := "0"
			switch mode {
			case "failure":
				want = "7"
			case "interrupt":
				want = "130"
				if _, err := master.Write([]byte{3}); err != nil {
					t.Fatal(err)
				}
			case "terminate":
				want = "143"
				pid, err := strconv.Atoi(scratchAwaitFile(t, filepath.Join(markers, "wrapper")))
				if err != nil {
					t.Fatal(err)
				}
				if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
				scratchAwaitFile(t, filepath.Join(markers, "finished"))
			}
			if got := scratchAwaitFile(t, filepath.Join(markers, "status")); got != want {
				t.Fatalf("terminal status = %q, want %q", got, want)
			}
			scratchAwaitFile(t, filepath.Join(markers, "restored"))
			if _, err := master.WriteString("after wrapper\n"); err != nil {
				t.Fatal(err)
			}
			if got := scratchAwaitFile(t, filepath.Join(markers, "after")); got != "after wrapper" {
				t.Fatalf("restored input = %q", got)
			}
			select {
			case <-done:
				if waitErr != nil {
					t.Fatal(waitErr)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("terminal shell survived completion")
			}
			scratch := scratchAwaitFile(t, filepath.Join(markers, "scratch"))
			if _, err := os.Stat(scratch); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("terminal invocation left scratch: %v", err)
			}
		})
	}
}

func TestBuildScratchFixtureCleanup(t *testing.T) {
	for _, mode := range []string{"graceful", "stubborn"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			_, script := buildScratchFixture(t)
			markers := t.TempDir()
			cmd := exec.Command("bash", script, "bash", "-c", `
ps -o pgid= -p "$$" > "$1/group"
printf '%s\n' "$TMPDIR" > "$1/scratch"
trap 'sleep 0.1; printf finished > "$1/finished"; exit 0' TERM
if test "$2" = stubborn; then trap '' TERM; fi
printf ready > "$1/ready"
while :; do sleep 0.1; done
`, "cleanup", markers, mode)
			run := startScratchCommand(t, cmd, markers)
			scratchAwaitFile(t, filepath.Join(markers, "ready"))
			// Invoke the exact callback used after Fatal/readiness failures while the
			// command is alive. Its TERM handler must run before forced termination.
			run.stop()
			if mode == "graceful" {
				scratchAwaitFile(t, filepath.Join(markers, "finished"))
			}
			scratch := scratchAwaitFile(t, filepath.Join(markers, "scratch"))
			if _, err := os.Stat(scratch); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("fixture cleanup left scratch: %v; wrapper: %v: %s", err, run.err, run.output.String())
			}
		})
	}
}
