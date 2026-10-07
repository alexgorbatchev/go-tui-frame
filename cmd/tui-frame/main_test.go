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

// Ctrl+Q only starts the child's termination; the wrapper stays transparent
// and returns however the child then ends, as it does when the child exits on
// its own.
func TestExecuteCtrlQStopsItsRealChild(t *testing.T) {
	for _, tt := range []struct {
		name string
		// child is the framed command; it creates ready once it runs.
		child func(ready string) []string
		code  int
	}{
		// The frame's SIGTERM ends the child: the shell's 128+15.
		{"terminated", func(ready string) []string {
			return []string{"sh", "-c", `printf started > "$1"; exec sleep 30`, "sh", ready}
		}, 143},
		// The child handles SIGTERM and exits with a status of its own.
		{"handles SIGTERM", func(ready string) []string {
			return []string{os.Args[0], "-test.run=^TestCLISIGTERMChild$"}
		}, 9},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ready := filepath.Join(projectTempDir(t), "child-started")
			t.Setenv("FRAME_CLI_SIGTERM_READY", ready)
			// A race-enabled child otherwise sleeps for the race detector's
			// default atexit_sleep_ms of 1000 when it exits, which lasts until
			// the frame's SIGKILL.
			t.Setenv("GORACE", "atexit_sleep_ms=0")
			// cliTTY replaces os.Args, which child reads.
			child := tt.child(ready)
			master, diagnostic := cliTTY(t, append([]string{"--showcase", "--"}, child...)...)
			done, exited := startExecute(t, master)
			awaitFile(t, ready, exited)
			if _, err := master.Write([]byte{0x11}); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-done:
				message, err := os.ReadFile(diagnostic)
				if err != nil {
					t.Fatal(err)
				}
				if got != tt.code || len(message) != 0 {
					t.Fatalf("Ctrl+Q returned %d with stderr %q; want the child's status %d and no diagnostic", got, message, tt.code)
				}
			case <-time.After(sessionTimeout):
				t.Fatal("Ctrl+Q did not finish the CLI")
			}
		})
	}
}

// TestCLISIGTERMChild runs as the framed child of
// TestExecuteCtrlQStopsItsRealChild's handles SIGTERM case. It handles SIGTERM
// from before it reports that it started and exits 9 when the signal arrives.
//
// It is one process, so the frame's group SIGTERM always reaches its handler.
// A shell that traps SIGTERM while it waits for a job can miss the trap: XNU's
// killpg signals a group's members one at a time, so the job can die and the
// shell exit before its own SIGTERM arrives, and a job still between fork and
// exec loses the signal to the shell's inherited handler.
func TestCLISIGTERMChild(t *testing.T) {
	ready := os.Getenv("FRAME_CLI_SIGTERM_READY")
	if ready == "" {
		return
	}
	terminated := make(chan os.Signal, 1)
	signal.Notify(terminated, syscall.SIGTERM)
	if err := os.WriteFile(ready, nil, 0600); err != nil {
		t.Fatal(err)
	}
	<-terminated
	os.Exit(9)
}

// frameInputQueue mirrors the library's inputQueueLimit: the most child input
// the frame queues before it holds outer input and pauses outer reads.
const frameInputQueue = 1 << 20

// A Ctrl+Q that the frame holds while its child still runs is captured only
// after the child has exited and been reaped. The CLI still returns the
// child's own status.
//
// The child puts its PTY in raw mode and never reads, so the frame's input
// queue fills. A paste leaves less room in that queue than the APC string that
// follows it, and the frame frames that string only once its terminator
// arrives. The terminator and Ctrl+Q are written together and so read
// together: both are held behind the string, and outer reads pause until the
// child exits.
func TestExecuteReturnsChildStatusWhenCtrlQFollowsItsExit(t *testing.T) {
	dir := projectTempDir(t)
	started, exit := filepath.Join(dir, "started"), filepath.Join(dir, "exit")
	script := `stty raw -echo; printf started > "$1"; while [ ! -e "$2" ]; do sleep 0.01; done; exit 7`
	master, diagnostic := cliTTY(t, "--", "sh", "-c", script, "sh", started, exit)
	outer := int(os.Stdin.Fd())
	code, exited := startExecute(t, master)
	awaitFile(t, started, exited)
	if err := master.SetWriteDeadline(time.Now().Add(sessionTimeout)); err != nil {
		t.Fatal(err)
	}
	const room = 8 << 10
	fill := "\x1b[200~" + strings.Repeat("a", frameInputQueue-room) + "\x1b[201~"
	held := "\x1b_" + strings.Repeat("x", 32*room)
	for _, input := range []string{fill, held} {
		if _, err := master.WriteString(input); err != nil {
			t.Fatal(err)
		}
		awaitOuterRead(t, outer, exited)
	}
	// String Terminator, then Ctrl+Q.
	if _, err := master.WriteString("\x1b\\\x11"); err != nil {
		t.Fatal(err)
	}
	awaitOuterRead(t, outer, exited)
	if err := os.WriteFile(exit, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-code:
		message, err := os.ReadFile(diagnostic)
		if err != nil {
			t.Fatal(err)
		}
		if got != 7 {
			t.Fatalf("execute returned %d, want the child's own status 7: %q", got, message)
		}
	case <-time.After(sessionTimeout):
		t.Fatal("CLI did not finish after its child exited")
	}
}

// awaitOuterRead waits until the CLI has read every byte written to its
// terminal, whose input descriptor is fd, failing if the CLI exits first. A
// zero-timeout poll reports unread input without consuming it.
func awaitOuterRead(t *testing.T, fd int, exited <-chan struct{}) {
	t.Helper()
	deadline := time.NewTimer(sessionTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		if _, err := unix.Poll(fds, 0); err != nil && !errors.Is(err, unix.EINTR) {
			t.Fatal(err)
		}
		if fds[0].Revents&unix.POLLIN == 0 {
			return
		}
		select {
		case <-tick.C:
		case <-exited:
			t.Fatal("CLI exited before it read its input")
		case <-deadline.C:
			t.Fatal("CLI left its input unread")
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
//
// The test depends on the library's terminationTimeout: the frame sends
// SIGKILL one second after its SIGTERM. The hangup and the child's after-quit
// write must both happen inside that window. If they do not, the child dies
// before the frame has failed to paint, and the test fails.
func TestExecuteCtrlQReportsShutdownFailures(t *testing.T) {
	dir := projectTempDir(t)
	t.Setenv("FRAME_CLI_QUIT_DIR", dir)
	t.Setenv("AGENT", "0")
	master, diagnostic := cliTTY(t, "--", os.Args[0], "-test.run=^TestCLIQuitChild$")
	status, exited := startExecute(t, master)
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
	var code int
	select {
	case code = <-status:
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

// startExecute runs execute on the terminal cliTTY set up, whose master side is
// master. It returns a channel that delivers execute's status and one that
// closes once execute returns. Cleanup closes master, hanging up the terminal
// as closing its tab does, and waits for execute to return.
func startExecute(t *testing.T, master *os.File) (<-chan int, <-chan struct{}) {
	t.Helper()
	status := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		status <- execute()
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
	return status, exited
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
