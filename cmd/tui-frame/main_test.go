package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func TestExecuteUsesNativeCobraAndChildExitStatuses(t *testing.T) {
	const missing = "/nonexistent/go-tui-frame-child"
	startFailure := "run child frame: start child PTY: fork/exec " + missing + ": no such file or directory"
	tests := []struct {
		name string
		args []string
		code int
		// report is the one error line execute writes after the mode's prefix;
		// empty means stderr stays empty.
		report string
	}{
		{"help", []string{"--help"}, 0, ""},
		{"version", []string{"--version"}, 0, ""},
		{"missing child", nil, 2, "put the child command after --, for example: tui-frame -- nvim"},
		{"unknown flag", []string{"--bogus"}, 2, "unknown flag: --bogus"},
		{"generated command validation", []string{"completion", "bash", "extra"}, 1, `unknown command "extra" for "tui-frame completion bash"`},
		{"missing executable", []string{"--", missing}, 1, startFailure},
		{"child success", []string{"--", "sh", "-c", "exit 0"}, 0, ""},
		{"showcase child success", []string{"--showcase", "--", "sh", "-c", "exit 0"}, 0, ""},
		{"showcase startup failure", []string{"--showcase", "--", missing}, 1, startFailure},
		// A child's own status is a result, not a wrapper failure.
		{"child failure", []string{"--", "sh", "-c", "exit 17"}, 17, ""},
		{"child signal", []string{"--", "sh", "-c", "kill -TERM $$"}, 143, ""},
	}
	for _, mode := range []struct{ agent, prefix string }{{"1", "ERR:"}, {"0", "[ERROR]"}} {
		for _, tt := range tests {
			t.Run("AGENT="+mode.agent+"/"+tt.name, func(t *testing.T) {
				_, diagnostic := cliTTY(t, tt.args...)
				t.Setenv("AGENT", mode.agent)
				got := execute()
				data, err := os.ReadFile(diagnostic)
				if err != nil {
					t.Fatal(err)
				}
				message := string(data)
				if got != tt.code {
					t.Fatalf("execute returned %d, want %d: %s", got, tt.code, message)
				}
				if tt.report == "" {
					if message != "" {
						t.Fatalf("stderr = %q, want no diagnostic", message)
					}
					return
				}
				var reported []string
				for _, line := range strings.Split(message, "\n") {
					if report, ok := strings.CutPrefix(line, mode.prefix+" "); ok {
						reported = append(reported, report)
					}
				}
				if len(reported) != 1 || reported[0] != tt.report {
					t.Fatalf("stderr reports %q, want only %q after %q: %q", reported, tt.report, mode.prefix, message)
				}
			})
		}
	}
}

func TestExecuteCtrlQStopsItsRealChild(t *testing.T) {
	ready := filepath.Join(projectTempDir(t), "child-started")
	master, _ := cliTTY(t, "--showcase", "--", "sh", "-c", `printf started > "$1"; exec sleep 30`, "sh", ready)
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- execute()
	}()
	t.Cleanup(func() {
		if err := master.Close(); err != nil {
			t.Errorf("close CLI terminal: %v", err)
		}
		select {
		case <-exited:
		case <-time.After(sessionTimeout):
			t.Error("CLI did not stop after terminal closure")
		}
	})
	deadline := time.NewTimer(sessionTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if _, err := os.Stat(ready); err == nil {
				if _, err := master.Write([]byte{0x11}); err != nil {
					t.Fatal(err)
				}
				select {
				case got := <-done:
					if got != 0 {
						t.Fatalf("Ctrl+Q returned %d", got)
					}
					return
				case <-deadline.C:
					t.Fatal("Ctrl+Q did not finish the CLI")
				}
			} else if !os.IsNotExist(err) {
				t.Fatal(err)
			}
		case got := <-done:
			t.Fatalf("CLI exited before its child started: %d", got)
		case <-deadline.C:
			t.Fatal("CLI did not start its child")
		}
	}
}

// TestCLIQuitChild runs as the framed child of
// TestExecuteCtrlQReportsShutdownFailures. It outlives the frame's SIGTERM
// until the outer terminal hangs up, then writes output the frame can no
// longer paint. It waits for the frame's SIGKILL instead of returning: the
// test result the binary would print next arrives after the failed paint ends
// the session, and Darwin does not finish a session leader's exit while its
// terminal holds unread output.
func TestCLIQuitChild(t *testing.T) {
	dir := os.Getenv("FRAME_CLI_QUIT_DIR")
	if dir == "" {
		return
	}
	terminated := make(chan os.Signal, 1)
	signal.Notify(terminated, syscall.SIGTERM)
	if err := os.WriteFile(filepath.Join(dir, "started"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	<-terminated
	if err := os.WriteFile(filepath.Join(dir, "terminated"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	awaitFile(t, filepath.Join(dir, "hung-up"), nil)
	if _, err := os.Stdout.WriteString("after-quit"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(sessionTimeout)
}

// After Ctrl+Q the frame keeps painting the child's output until the child
// exits, then restores the terminal. Failures in that shutdown are session
// errors, so the quit request must not hide them.
func TestExecuteCtrlQReportsShutdownFailures(t *testing.T) {
	dir := projectTempDir(t)
	t.Setenv("FRAME_CLI_QUIT_DIR", dir)
	t.Setenv("AGENT", "0")
	master, diagnostic := cliTTY(t, "--", os.Args[0], "-test.run=^TestCLIQuitChild$")
	var code int
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		code = execute()
	}()
	t.Cleanup(func() {
		if err := master.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Errorf("close CLI terminal: %v", err)
		}
		select {
		case <-exited:
		case <-time.After(sessionTimeout):
			t.Error("CLI did not stop after terminal closure")
		}
	})
	awaitFile(t, filepath.Join(dir, "started"), exited)
	if _, err := master.Write([]byte{0x11}); err != nil {
		t.Fatal(err)
	}
	awaitFile(t, filepath.Join(dir, "terminated"), exited)
	if err := master.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hung-up"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(sessionTimeout):
		t.Fatal("Ctrl+Q did not finish the CLI")
	}
	data, err := os.ReadFile(diagnostic)
	if err != nil {
		t.Fatal(err)
	}
	message := string(data)
	if code != 1 {
		t.Fatalf("execute returned %d, want 1: %q", code, message)
	}
	var reports []string
	for _, line := range strings.Split(message, "\n") {
		if report, ok := strings.CutPrefix(line, "[ERROR] "); ok {
			reports = append(reports, report)
		}
	}
	if len(reports) != 1 || !strings.HasPrefix(reports[0], "run child frame: ") {
		t.Fatalf("stderr reports %q, want one run child frame error: %q", reports, message)
	}
	for _, want := range []string{"render outer terminal: ", "restore terminal modes: "} {
		if !strings.Contains(message, want) {
			t.Errorf("stderr %q does not report %q", message, want)
		}
	}
	if strings.Contains(message, errDemoQuit.Error()) {
		t.Errorf("stderr %q reports the quit request as a failure", message)
	}
}

// awaitFile waits until path exists, failing if the CLI exits first. A nil
// exited channel waits without watching a CLI.
func awaitFile(t *testing.T, path string, exited <-chan struct{}) {
	t.Helper()
	deadline := time.NewTimer(sessionTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		_, err := os.Stat(path)
		if err == nil {
			return
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case <-tick.C:
		case <-exited:
			t.Fatalf("CLI exited before %s appeared", filepath.Base(path))
		case <-deadline.C:
			t.Fatalf("%s did not appear", filepath.Base(path))
		}
	}
}

// TestCLISignalChild runs as the framed child of
// TestExecuteSignalsCancelTheSession. Its descendant leads a separate process
// group inside the child's PTY session, so stopping it requires the frame to
// terminate every group in that session rather than only the leader's.
func TestCLISignalChild(t *testing.T) {
	path := os.Getenv("FRAME_CLI_SIGNAL_PID_FILE")
	if path == "" {
		return
	}
	descendant := exec.Command("sleep", "60")
	descendant.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := descendant.Start(); err != nil {
		t.Fatal(err)
	}
	// Rename publishes both PIDs to the polling parent in one step.
	pids := fmt.Sprintf("%d %d", os.Getpid(), descendant.Process.Pid)
	if err := os.WriteFile(path+".pending", []byte(pids), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".pending", path); err != nil {
		t.Fatal(err)
	}
	if err := descendant.Wait(); err != nil {
		t.Fatal(err)
	}
}

// The wrapper runs as a separate process because an unhandled SIGHUP, SIGINT,
// or SIGTERM would terminate the test binary itself.
func TestExecuteSignalsCancelTheSession(t *testing.T) {
	binary := buildExample(t)
	for _, tt := range []struct {
		name string
		sig  syscall.Signal
		// closed hangs up the outer PTY before the signal, as closing the
		// terminal tab does, so restoring it fails during the shutdown.
		closed bool
		// code is the shell's 128+N status for the cancelling signal.
		code int
	}{
		{"hangup", syscall.SIGHUP, false, 129},
		{"hangup after terminal closed", syscall.SIGHUP, true, 129},
		{"interrupt", syscall.SIGINT, false, 130},
		{"terminated", syscall.SIGTERM, false, 143},
	} {
		t.Run(tt.name, func(t *testing.T) {
			master, slave := demoTTY(t)
			before, err := term.GetState(int(slave.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			dir := projectTempDir(t)
			diagnostic := filepath.Join(dir, "diagnostic")
			stderr, err := os.Create(diagnostic)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := stderr.Close(); err != nil {
					t.Errorf("close wrapper diagnostic: %v", err)
				}
			})
			pidFile := filepath.Join(dir, "pids")
			wrapper := exec.Command(binary, "--", os.Args[0], "-test.run=^TestCLISignalChild$")
			wrapper.Env = append(os.Environ(), "FRAME_CLI_SIGNAL_PID_FILE="+pidFile)
			wrapper.Stdin, wrapper.Stdout, wrapper.Stderr = slave, slave, stderr
			if err := wrapper.Start(); err != nil {
				t.Fatal(err)
			}
			var waitErr error
			exited := make(chan struct{})
			go func() {
				defer close(exited)
				waitErr = wrapper.Wait()
			}()
			t.Cleanup(func() {
				select {
				case <-exited:
					return
				default:
				}
				if err := wrapper.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
					t.Errorf("kill wrapper: %v", err)
				}
				<-exited
			})
			leader, descendant := awaitSignalChild(t, pidFile, exited)
			if tt.closed {
				if err := master.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if err := wrapper.Process.Signal(tt.sig); err != nil {
				t.Fatal(err)
			}
			select {
			case <-exited:
			case <-time.After(sessionTimeout):
				t.Fatalf("wrapper did not exit after %v", tt.sig)
			}
			message, err := os.ReadFile(diagnostic)
			if err != nil {
				t.Fatal(err)
			}
			var exit *exec.ExitError
			if !errors.As(waitErr, &exit) {
				t.Fatalf("wrapper wait = %v; want its execute status: %s", waitErr, message)
			}
			// A handled signal ends the session through execute, which reports
			// the signal's shell status; the default action would terminate the
			// wrapper before the frame's shutdown.
			if status, ok := exit.Sys().(syscall.WaitStatus); !ok || status.Signaled() || status.ExitStatus() != tt.code {
				t.Errorf("wrapper status = %v; want exit status %d from execute: %s", exit.ProcessState, tt.code, message)
			}
			if tt.closed {
				if !strings.Contains(string(message), "restore terminal modes") {
					t.Errorf("wrapper diagnostic %q does not report the failed restoration", message)
				}
			} else {
				if want := tt.sig.String() + " signal received"; !strings.Contains(string(message), want) {
					t.Errorf("wrapper diagnostic %q does not contain %q", message, want)
				}
				if after, err := term.GetState(int(slave.Fd())); err != nil || !reflect.DeepEqual(before, after) {
					t.Errorf("outer terminal state was not restored after %v: %v", tt.sig, err)
				}
			}
			awaitGone(t, -leader, "child leader's process group survived "+tt.name)
			awaitGone(t, -descendant, "descendant process group survived "+tt.name)
		})
	}
}

// awaitSignalChild reads the PIDs TestCLISignalChild publishes and verifies
// that the descendant leads its own process group in the leader's session.
func awaitSignalChild(t *testing.T, file string, exited <-chan struct{}) (int, int) {
	t.Helper()
	deadline := time.NewTimer(sessionTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
		case <-exited:
			t.Fatal("wrapper exited before its child started")
		case <-deadline.C:
			t.Fatal("framed child did not publish its PIDs")
		}
		data, err := os.ReadFile(file)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		var leader, descendant int
		if _, err := fmt.Sscan(string(data), &leader, &descendant); err != nil {
			t.Fatalf("parse child PIDs %q: %v", data, err)
		}
		for _, pgid := range []int{leader, descendant} {
			t.Cleanup(func() {
				if err := unix.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
					t.Errorf("kill process group %d: %v", pgid, err)
				}
			})
		}
		pgid, pgidErr := unix.Getpgid(descendant)
		sid, sidErr := unix.Getsid(descendant)
		if pgidErr != nil || sidErr != nil || pgid != descendant || sid != leader {
			t.Fatalf("descendant %d group=%d (%v) session=%d (%v); want its own group in session %d", descendant, pgid, pgidErr, sid, sidErr, leader)
		}
		return leader, descendant
	}
}

// awaitGone waits until kill(2) finds no process for target, a PID or a
// negated process-group ID. Darwin reports EPERM for a group whose only
// members are unreaped zombies, so only ESRCH proves the group is gone.
func awaitGone(t *testing.T, target int, failure string) {
	t.Helper()
	deadline := time.Now().Add(sessionTimeout)
	var err error
	for time.Now().Before(deadline) {
		if err = unix.Kill(target, 0); errors.Is(err, unix.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("%s: kill(%d, 0) = %v", failure, target, err)
}

func cliTTY(t *testing.T, args ...string) (*os.File, string) {
	t.Helper()
	master, slave := demoTTY(t)
	diagnostic, err := os.CreateTemp(projectTempDir(t), "diagnostic")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := diagnostic.Close(); err != nil {
			t.Errorf("close diagnostic: %v", err)
		}
	})
	originalIn, originalOut, originalErr, originalArgs := os.Stdin, os.Stdout, os.Stderr, os.Args
	os.Stdin, os.Stdout, os.Stderr = slave, slave, diagnostic
	os.Args = append([]string{"tui-frame"}, args...)
	t.Cleanup(func() { os.Stdin, os.Stdout, os.Stderr, os.Args = originalIn, originalOut, originalErr, originalArgs })
	return master, diagnostic.Name()
}

func TestExecutePreservesEveryChildArgument(t *testing.T) {
	output := filepath.Join(projectTempDir(t), "child-argv")
	want := []string{"--help", "word two", "", "--", "-v", "$literal"}
	args := []string{"--", "sh", "-c", `out=$1; shift; printf '%s\000' "$@" > "$out"`, "sh", output}
	cliTTY(t, append(args, want...)...)
	if code := execute(); code != 0 {
		t.Fatalf("argv child returned %d", code)
	}
	got, err := os.ReadFile(output)
	if err != nil || string(got) != strings.Join(want, "\x00")+"\x00" {
		t.Fatalf("child argv changed: %q, %v", got, err)
	}
}
