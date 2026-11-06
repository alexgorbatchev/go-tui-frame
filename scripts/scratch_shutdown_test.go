package scripts

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestBuildScratchForcedShutdown(t *testing.T) {
	_, script := buildScratchFixture(t)
	for _, mode := range []string{"running", "stopped", "orphan"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			markers := t.TempDir()
			cmd := exec.Command("bash", script, "bash", "-c", `set -eu
ps -o pgid= -p "$$" > "$1/group"
printf '%s\n' "$TMPDIR" > "$1/scratch"
bash -c '
trap "" TERM
printf ready > "$1/ready"
if test "$2" = stopped; then kill -STOP "$$"; fi
exec sleep 60
' stubborn "$1" "$2" &
while ! test -s "$1/ready"; do sleep 0.01; done
if test "$2" = orphan; then exit 0; fi
wait
`, "leader", markers, mode)
			run := startScratchCommand(t, cmd, markers)
			scratchAwaitFile(t, filepath.Join(markers, "ready"))
			status := 1
			if mode != "orphan" {
				status = 143
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			}
			run.wait(t, status)
			scratch := scratchAwaitFile(t, filepath.Join(markers, "scratch"))
			if _, err := os.Stat(scratch); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("scratch survived confirmed forced shutdown: %v", err)
			}
		})
	}
}

func TestBuildScratchRetainsOnInspectionFailure(t *testing.T) {
	for name, body := range map[string]string{
		"failed":    "exit 7\n",
		"empty":     "exit 0\n",
		"malformed": "printf 'unknown\\n'\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root, script := buildScratchFixture(t)
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "ps"), []byte("#!/bin/sh\n"+body), 0o700); err != nil {
				t.Fatal(err)
			}
			// The real child finishes, but a failed ps cannot prove the group
			// is absent. Its owned scratch must remain available for diagnosis.
			cmd := exec.Command("bash", script, "sh", "-c", `printf '%s\n' "$TMPDIR"`)
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
			output, err := cmd.Output()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(exit.Stderr), "not confirmed stopped; retained") {
				t.Fatalf("inspection failure: %v: %s", err, output)
			}
			scratch := strings.TrimSpace(string(output))
			if filepath.Dir(scratch) != filepath.Join(root, ".tmp") {
				t.Fatalf("unexpected retained scratch %q", scratch)
			}
			if _, err := os.Stat(scratch); err != nil {
				t.Fatalf("uncertain shutdown deleted scratch: %v", err)
			}
		})
	}
}

func TestBuildScratchInputAndLaunchFailure(t *testing.T) {
	for _, args := range [][]string{nil, {"no-such-build-command"}, {"cat"}} {
		t.Run(strings.Join(args, "/"), func(t *testing.T) {
			root, script := buildScratchFixture(t)
			cmd := exec.Command("bash", append([]string{script}, args...)...)
			cmd.Stdin = strings.NewReader("build input\n")
			output, err := cmd.Output()
			if len(args) != 0 && args[0] == "cat" {
				if err != nil || string(output) != "build input\n" {
					t.Fatalf("stdin passthrough: %v: %q", err, output)
				}
			} else if err == nil {
				t.Fatal("invalid invocation reported success")
			}
			entries, err := os.ReadDir(filepath.Join(root, ".tmp"))
			if err != nil && !errors.Is(err, os.ErrNotExist) || len(entries) != 0 {
				t.Fatalf("invocation left scratch: %v, %v", entries, err)
			}
		})
	}
}
