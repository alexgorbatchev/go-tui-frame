package frame

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/alexgorbatchev/go-tui-frame/internal/emulator"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/term"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// The child runs in a separate real PTY and uses the slave's native line discipline.
func TestSessionChild(t *testing.T) {
	if os.Getenv("FRAME_TEST_CHILD") != "1" {
		return
	}
	if os.Getenv("FRAME_TEST_MODE") == "sleep" {
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	if os.Getenv("FRAME_TEST_MODE") == "groups" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestSessionChild$")
		cmd.Env = append(os.Environ(), "FRAME_TEST_MODE=sleep")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Start(); err != nil {
			os.Exit(95)
		}
		pidFile := os.Getenv("FRAME_TEST_PID_FILE")
		if err := os.WriteFile(pidFile+".pending", []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
			os.Exit(96)
		}
		if err := os.Rename(pidFile+".pending", pidFile); err != nil {
			os.Exit(98)
		}
		if err := cmd.Wait(); err != nil {
			os.Exit(97)
		}
		os.Exit(0)
	}
	if _, err := term.MakeRaw(os.Stdin.Fd()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(91)
	}
	if _, err := fmt.Fprint(os.Stdout, "\x1b[2J\x1b[Hchild\x1b]2;Child title\x07\x1b[6n"); err != nil {
		os.Exit(92)
	}
	buf := make([]byte, 256)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			os.Exit(93)
		}
		if strings.Contains(string(buf[:n]), "q") {
			if _, err := fmt.Fprint(os.Stdout, "\x1b[2;1Hfinished"); err != nil {
				os.Exit(94)
			}
			os.Exit(0)
		}
	}
}

type terminalHarness struct {
	master, slave *os.File
	fd            int
	mu            sync.Mutex
	em            *emulator.Terminal
	stop, done    chan struct{}
	err           error
}

func newHarness(t *testing.T) *terminalHarness {
	t.Helper()
	m, s, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := pty.Setsize(s, &pty.Winsize{Cols: 40, Rows: 12, X: 400, Y: 240}); err != nil {
		t.Fatal(err)
	}
	em, err := emulator.New(emulator.Options{Size: emulator.Size{Cols: 40, Rows: 12, CellWidthPx: 10, CellHeightPx: 20}, GraphemeWidth: true})
	if err != nil {
		t.Fatal(err)
	}
	h := &terminalHarness{master: m, slave: s, fd: int(m.Fd()), em: em, stop: make(chan struct{}), done: make(chan struct{})}
	if err := unix.SetNonblock(h.fd, true); err != nil {
		t.Fatal(err)
	}
	go h.read()
	t.Cleanup(func() {
		close(h.stop)
		<-h.done
		h.mu.Lock()
		err := h.err
		h.em.Close()
		h.mu.Unlock()
		if err != nil {
			t.Errorf("outer terminal: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Error(err)
		}
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	})
	return h
}

func (h *terminalHarness) read() {
	defer close(h.done)
	buf := make([]byte, 65536)
	for {
		select {
		case <-h.stop:
			return
		default:
		}
		fds := []unix.PollFd{{Fd: int32(h.fd), Events: unix.POLLIN}}
		if _, err := unix.Poll(fds, 20); err != nil && !errors.Is(err, unix.EINTR) {
			h.fail(err)
			return
		}
		if fds[0].Revents&unix.POLLIN == 0 {
			continue
		}
		n, err := unix.Read(h.fd, buf)
		if errors.Is(err, unix.EAGAIN) {
			continue
		}
		if err != nil {
			h.fail(err)
			return
		}
		h.mu.Lock()
		_, err = h.em.Write(buf[:n])
		var replies []byte
		for _, e := range h.em.Effects() {
			if e.Kind == emulator.Reply {
				replies = append(replies, e.Bytes...)
			}
		}
		h.mu.Unlock()
		if err != nil {
			h.fail(err)
			return
		}
		if len(replies) > 0 {
			if _, err := unix.Write(h.fd, replies); err != nil {
				h.fail(err)
				return
			}
		}
	}
}

func (h *terminalHarness) fail(err error) { h.mu.Lock(); h.err = err; h.mu.Unlock() }

func (h *terminalHarness) text() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, err := h.em.State()
	if err != nil {
		return "state error: " + err.Error()
	}
	var b strings.Builder
	for _, c := range s.Cells {
		b.WriteString(c.Content)
	}
	return b.String()
}

func awaitText(t *testing.T, h *terminalHarness, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(h.text(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("outer display missing %q: %q", want, h.text())
}

func childCommand(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSessionChild$")
	cmd.Env = append(os.Environ(), "FRAME_TEST_CHILD=1", "TERM=xterm-256color")
	return cmd
}

func TestRunFramesRealChildAndPushesIdleUpdate(t *testing.T) {
	h := newHarness(t)
	before, err := term.GetState(h.slave.Fd())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	observed := make(chan struct{})
	snapshots := make(chan Snapshot, 1)
	app := New(childCommand(t), "Ready").Terminal(h.slave, h.slave).
		Header(1, func(c DrawContext[string]) {
			uv.NewStyledString(c.Data).Draw(c.View, c.View.Bounds())
			if c.Data == "Pushed while idle" {
				select {
				case snapshots <- c.Term:
				default:
				}
			}
		}).
		Footer(1, func(c DrawContext[string]) { uv.NewStyledString(c.Term.Terminal.Title).Draw(c.View, c.View.Bounds()) }).Border(true).
		Observe(func(e Event) {
			if e.Kind != Started {
				return
			}
			// Observer-owned containers must not corrupt a later drawing snapshot.
			s := e.Snapshot
			if s.Child.PTY.Window != nil {
				s.Child.PTY.Window.Col = 1
			}
			if len(s.Child.OperatingSystem.Args.Value) > 0 {
				s.Child.OperatingSystem.Args.Value[0] = "observer-owned"
			}
			if len(s.Terminal.Native.Cells) > 0 {
				s.Terminal.Native.Cells[0].Content = "observer-owned"
			}
			close(observed)
		})
	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() { r, err := app.Run(ctx); done <- outcome{r, err} }()
	awaitText(t, h, "Ready")
	awaitText(t, h, "child")
	awaitText(t, h, "Child title")
	select {
	case <-observed:
	case <-ctx.Done():
		t.Fatal("Started was not observed", ctx.Err())
	}
	if err := app.InvalidateHeader("Pushed while idle"); err != nil {
		t.Fatal(err)
	}
	awaitText(t, h, "Pushed while idle")
	select {
	case s := <-snapshots:
		p := s.Child.OperatingSystem
		if p.PID != s.Child.PID || !p.SessionID.Available || p.SessionID.Value != s.Child.PID || !p.ProcessGroup.Available || p.ProcessGroup.Value != s.Child.PID {
			t.Fatalf("native child identity: %#v", p)
		}
		if !p.Args.Available || len(p.Args.Value) == 0 || p.Args.Value[0] != s.Child.Args[0] {
			t.Fatalf("native argv: %#v", p.Args)
		}
		found := false
		for _, member := range s.Child.SessionProcesses {
			found = found || member.PID == s.Child.PID
		}
		if !found || s.Child.SessionError != nil {
			t.Fatalf("owned session inventory: %#v, %v", s.Child.SessionProcesses, s.Child.SessionError)
		}
		pty := s.Child.PTY
		if pty.Window == nil || int(pty.Window.Col) != s.Viewport.Cols || int(pty.Window.Row) != s.Viewport.Rows || pty.Settings == nil || !pty.ForegroundGroup.Available || pty.ForegroundGroup.Value != s.Child.PID {
			t.Fatalf("native PTY metadata: %#v", pty)
		}
		if s.Terminal.Title != "Child title" || len(s.Terminal.Native.Cells) == 0 || s.Terminal.Native.Cells[0].Content == "observer-owned" {
			t.Fatalf("owned terminal snapshot: %#v", s.Terminal)
		}
	case <-ctx.Done():
		t.Fatal("pushed drawing snapshot was not delivered", ctx.Err())
	}
	if _, err := unix.Write(h.fd, []byte("q")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil || got.result.ProcessState == nil || !got.result.ProcessState.Success() || got.result.DrainError != nil || got.result.CleanupError != nil {
			t.Fatalf("Run = %#v, %v", got.result, got.err)
		}
	case <-ctx.Done():
		t.Fatal("Run did not finish", ctx.Err())
	}
	after, err := term.GetState(h.slave.Fd())
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%#v", before) != fmt.Sprintf("%#v", after) {
		t.Fatalf("termios was not restored: before %#v after %#v", before, after)
	}
	if err := app.InvalidateHeader("closed"); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("closed invalidation: %v", err)
	}
}

func TestCancellationRestoresTerminalBeforeObserverDrains(t *testing.T) {
	h := newHarness(t)
	before, err := term.GetState(h.slave.Fd())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	started, release := make(chan struct{}), make(chan struct{})
	app := New(childCommand(t), "Ready").Terminal(h.slave, h.slave).Header(1, func(c DrawContext[string]) { uv.NewStyledString(c.Data).Draw(c.View, c.View.Bounds()) }).Observe(func(e Event) {
		if e.Kind == Started {
			close(started)
			<-release
		}
	})
	done := make(chan error, 1)
	go func() { _, err := app.Run(ctx); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("child was not started")
	}
	cause := errors.New("test cancellation")
	cancel(cause)
	deadline := time.Now().Add(3 * time.Second)
	restored := false
	for time.Now().Before(deadline) {
		after, e := term.GetState(h.slave.Fd())
		if e == nil && fmt.Sprintf("%#v", before) == fmt.Sprintf("%#v", after) {
			restored = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	closed := errors.Is(app.InvalidateHeader("late"), ErrSessionClosed)
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, cause) {
			t.Errorf("cancellation cause: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("observer shutdown was not joined")
	}
	if !restored || !closed {
		t.Fatalf("terminal restored before observer drain=%v, controller closed=%v", restored, closed)
	}
}

func TestCancellationTerminatesAllOwnedSessionGroups(t *testing.T) {
	h := newHarness(t)
	dir, err := os.MkdirTemp(".tmp", "session-groups-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	file := dir + "/pid"
	cmd := childCommand(t)
	cmd.Env = append(cmd.Env, "FRAME_TEST_MODE=groups", "FRAME_TEST_PID_FILE="+file)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan int, 1)
	done := make(chan error, 1)
	go func() {
		_, err := New(cmd, struct{}{}).Terminal(h.slave, h.slave).Observe(func(e Event) {
			if e.Kind == Started {
				started <- e.Snapshot.Child.PID
			}
		}).Run(ctx)
		done <- err
	}()
	var leader int
	select {
	case leader = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("leader did not start")
	}
	deadline := time.Now().Add(5 * time.Second)
	pid := 0
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(file)
		if err == nil {
			pid, err = strconv.Atoi(string(data))
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("separate child group did not start")
	}
	t.Cleanup(func() {
		if err := unix.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
			t.Error(err)
		}
	})
	if pgid, err := unix.Getpgid(pid); err != nil || pgid != pid || pgid == leader {
		t.Fatalf("group = %d, %v; leader=%d descendant=%d", pgid, err, leader, pid)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("cancel result: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled session did not complete")
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := unix.Kill(pid, 0); errors.Is(err, unix.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("owned descendant in separate process group survived cancellation")
}
