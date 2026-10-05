package frame

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/alexgorbatchev/go-tui-frame/internal/emulator"
	"github.com/alexgorbatchev/go-tui-frame/internal/input"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// A child that stops reading its input holds outer input in the terminal, as
// a terminal writing straight to the child's PTY would, instead of ending the
// session. Kitty-encoded keys check that routing expansion stays bounded.
func TestOuterInputWaitsForBusyChild(t *testing.T) {
	const keys = 90_000
	paste := bracketedPaste(5 * inputQueueLimit / 2)
	for _, tt := range []struct {
		name, childModes string
		input, want      []byte
	}{
		{name: "bracketed paste", childModes: "\x1b[?2004h", input: paste, want: paste},
		// UV reports "Z" as Shift with its shifted text. Kitty alternates and
		// associated text turn each such byte into 14 child-input bytes.
		{name: "Kitty-encoded keys", childModes: "\x1b[>20u", input: bytes.Repeat([]byte("Z"), keys), want: bytes.Repeat([]byte("\x1b[122:90;2;90u"), keys)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, outer, child := newQueueSession(t, tt.childModes)
			_, written := feedOuter(t, outer, tt.input)
			deadline := time.Now().Add(30 * time.Second)
			wakeAt(t, s, deadline)
			buf := make([]byte, readBufferSize)
			for {
				paused, _ := step(t, s, buf)
				if paused && inputWaiting(t, s.console.fd) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("outer reads never paused with input waiting in the terminal; queued %d", s.queuedBytes)
				}
			}
			received := collectChild(t, s, child, len(tt.want))
			for {
				step(t, s, buf)
				select {
				case got := <-received:
					requireSameBytes(t, got, tt.want)
					select {
					case err := <-written:
						if err != nil {
							t.Fatal(err)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("outer terminal did not finish writing its input")
					}
					return
				default:
				}
				if time.Now().After(deadline) {
					t.Fatalf("child did not receive its input; queued %d", s.queuedBytes)
				}
			}
		})
	}
}

// Paused outer input must keep reporting a terminal hangup, which ends a
// session whose child is not reading. The hangup arrives while the session is
// already waiting in poll.
func TestPausedOuterInputReportsHangup(t *testing.T) {
	s, outer, _ := newQueueSession(t, "\x1b[?2004h")
	stop, _ := feedOuter(t, outer, bracketedPaste(2*inputQueueLimit))
	deadline := time.Now().Add(30 * time.Second)
	wakeAt(t, s, deadline)
	buf := make([]byte, readBufferSize)
	for {
		paused, _ := step(t, s, buf)
		if paused && inputWaiting(t, s.console.fd) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("outer reads never paused with input waiting in the terminal; queued %d", s.queuedBytes)
		}
	}
	stop()
	const limit = time.Second
	closed := make(chan error, 1)
	start := time.Now()
	go func() {
		time.Sleep(50 * time.Millisecond)
		closed <- outer.Close()
	}()
	wakeAt(t, s, start.Add(3*limit))
	for {
		paused, disconnected := step(t, s, buf)
		if !paused {
			t.Fatal("outer reads resumed although the child read nothing")
		}
		if disconnected {
			break
		}
		if time.Since(start) > 3*limit {
			t.Fatal("outer terminal hangup was not reported while input was paused")
		}
	}
	if elapsed := time.Since(start); elapsed > limit {
		t.Fatalf("hangup reported after %v, want under %v", elapsed, limit)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

// An Escape that ends the read which paused outer input cannot time out into a
// bare Escape while the bytes completing its sequence wait unread.
func TestPausedOuterInputKeepsEscapeSequences(t *testing.T) {
	// Kitty disambiguation makes a timed-out Escape "\x1b[27u", unlike the
	// Up arrow "\x1b[A" whose first byte it is.
	s, outer, child := newQueueSession(t, "\x1b[>1u")
	want := fillChild(t, s, inputQueueLimit-10)
	want = append(want, "ab\x1b[A"...)
	if _, err := outer.WriteString("ab\x1b"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	wakeAt(t, s, deadline)
	buf := make([]byte, readBufferSize)
	step(t, s, buf)
	for start := time.Now(); time.Since(start) < 3*escapeTimeout; {
		if paused, _ := step(t, s, buf); !paused || !s.framer.Pending() {
			t.Fatalf("paused=%v with pending Escape=%v; want both while the child reads nothing", paused, s.framer.Pending())
		}
	}
	if _, err := outer.WriteString("[A"); err != nil {
		t.Fatal(err)
	}
	received := collectChild(t, s, child, len(want))
	for {
		step(t, s, buf)
		select {
		case got := <-received:
			requireSameBytes(t, got, want)
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("child did not receive its input; queued %d", s.queuedBytes)
		}
	}
}

// newQueueSession builds a session between a real outer terminal PTY, whose
// master the test writes, and a child PTY whose slave nothing reads until the
// test collects it. childModes sets the child's native input modes.
func newQueueSession(t *testing.T, childModes string) (s *session[struct{}], outer, child *os.File) {
	t.Helper()
	outer, outerSlave := rawPTY(t)
	childMaster, child := rawPTY(t)
	// File.Fd can restore blocking mode on pollable files. Cache the
	// descriptor before SetNonblock, as the production session does.
	childFD := int(childMaster.Fd())
	if err := unix.SetNonblock(childFD, true); err != nil {
		t.Fatal(err)
	}
	f := New(exec.Command("sh"), struct{}{})
	g, err := f.layout(Size{Cols: 20, Rows: 6})
	if err != nil {
		t.Fatal(err)
	}
	em, err := emulator.New(emulator.Options{Size: emulator.Size{Cols: g.child.Dx(), Rows: g.child.Dy()}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(em.Close)
	if _, err := em.Write([]byte(childModes)); err != nil {
		t.Fatal(err)
	}
	router, err := newInputRouter(em, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(router.close)
	c := &console{fd: int(outerSlave.Fd()), framer: input.New(), renderer: uv.NewTerminalRenderer(io.Discard, []string{"TERM=xterm-256color"}),
		entry: make(map[ansi.DECMode]ansi.ModeSetting), applied: make(map[ansi.DECMode]bool)}
	s = &session[struct{}]{frame: f, console: c, terminal: em, router: router, geometry: g, screen: c.outerScreen(g), framer: c.framer, fd: childFD}
	if err := s.openWake(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.closeWake(); err != nil {
			t.Error(err)
		}
	})
	if err := s.refresh(true); err != nil {
		t.Fatal(err)
	}
	return s, outer, child
}

// rawPTY opens a PTY whose slave passes bytes unchanged, as the frame's raw
// outer terminal and a raw child do.
func rawPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, f := range []*os.File{master, slave} {
			if err := f.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				t.Error(err)
			}
		}
	})
	if _, err := term.MakeRaw(slave.Fd()); err != nil {
		t.Fatal(err)
	}
	return master, slave
}

// step runs one iteration of the session loop without a failure: deadlines,
// one poll, and every ready descriptor. It reports whether the poll watched
// the outer terminal without requesting its input, and whether the terminal
// disconnected. Any session error or queue overrun fails the test.
func step(t *testing.T, s *session[struct{}], buf []byte) (paused, disconnected bool) {
	t.Helper()
	if err := s.deadlines(); err != nil {
		t.Fatal(err)
	}
	fds := s.pollFDs(nil)
	paused = fds[1].Fd == int32(s.console.fd) && fds[1].Events&unix.POLLIN == 0
	disconnected, err := s.await(buf, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.queuedBytes > inputQueueLimit {
		t.Fatalf("queued %d child-input bytes, over the %d-byte limit", s.queuedBytes, inputQueueLimit)
	}
	return paused, disconnected
}

// wakeAt wakes the session loop at when, so a step blocked in poll returns
// and the test can fail at its deadline instead of hanging.
func wakeAt(t *testing.T, s *session[struct{}], when time.Time) {
	t.Helper()
	timer := time.AfterFunc(time.Until(when), s.wakeLoop)
	t.Cleanup(func() { timer.Stop() })
}

// feedOuter writes data to the outer terminal on another goroutine, waiting
// whenever the terminal's input buffer is full, as a terminal delivering a
// paste does. stop abandons the rest and returns once the writer has stopped;
// written reports the writer's outcome once.
func feedOuter(t *testing.T, master *os.File, data []byte) (stop func(), written <-chan error) {
	t.Helper()
	fd := int(master.Fd())
	if err := unix.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	quit, done, result := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		defer close(done)
		result <- pumpPTY(quit, fd, unix.POLLOUT, func() (bool, error) {
			n, err := unix.Write(fd, data)
			if n > 0 {
				data = data[n:]
			}
			return len(data) == 0, err
		})
	}()
	stopped := false
	stop = func() {
		if !stopped {
			stopped = true
			close(quit)
			<-done
		}
	}
	t.Cleanup(stop)
	return stop, result
}

// collectChild reads want bytes from the child's PTY slave on another
// goroutine, as a child that resumes reading does, then wakes the session loop
// and delivers them.
func collectChild(t *testing.T, s *session[struct{}], slave *os.File, want int) <-chan []byte {
	t.Helper()
	fd := int(slave.Fd())
	if err := unix.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	quit, done, received := make(chan struct{}), make(chan struct{}), make(chan []byte, 1)
	go func() {
		defer close(done)
		got := make([]byte, 0, want)
		buf := make([]byte, readBufferSize)
		err := pumpPTY(quit, fd, unix.POLLIN, func() (bool, error) {
			n, err := unix.Read(fd, buf)
			if n > 0 {
				got = append(got, buf[:n]...)
			}
			return len(got) >= want, err
		})
		if err == nil && len(got) >= want {
			received <- got
			s.wakeLoop()
		}
	}()
	t.Cleanup(func() { close(quit); <-done })
	return received
}

// pumpPTY repeats transfer on a nonblocking PTY descriptor until it reports
// completion or quit closes, polling for events while the PTY is not ready.
func pumpPTY(quit <-chan struct{}, fd int, events int16, transfer func() (bool, error)) error {
	for {
		select {
		case <-quit:
			return nil
		default:
		}
		finished, err := transfer()
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			fds := []unix.PollFd{{Fd: int32(fd), Events: events}}
			if _, err := unix.Poll(fds, 20); err != nil && !errors.Is(err, unix.EINTR) {
				return err
			}
			continue
		}
		if err != nil || finished {
			return err
		}
	}
}

// fillChild queues n filler bytes and writes them until the child's PTY takes
// no more, then tops the queue up to n bytes again. It returns every filler
// byte the child will read.
func fillChild(t *testing.T, s *session[struct{}], n int) []byte {
	t.Helper()
	fill := bytes.Repeat([]byte("x"), n)
	if err := s.enqueue(fill, "user-input"); err != nil {
		t.Fatal(err)
	}
	for {
		queued := s.queuedBytes
		if err := s.writeInput(); err != nil {
			t.Fatal(err)
		}
		if s.queuedBytes == queued {
			break
		}
	}
	top := bytes.Repeat([]byte("x"), n-s.queuedBytes)
	if err := s.enqueue(top, "user-input"); err != nil {
		t.Fatal(err)
	}
	return append(fill, top...)
}

// inputWaiting reports whether bytes wait unread in the terminal's input queue.
func inputWaiting(t *testing.T, fd int) bool {
	t.Helper()
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	if _, err := unix.Poll(fds, 0); err != nil {
		t.Fatal(err)
	}
	return fds[0].Revents&unix.POLLIN != 0
}

// bracketedPaste returns a marked paste of at least n bytes. Its numbered lines
// expose lost, repeated and reordered bytes.
func bracketedPaste(n int) []byte {
	b := []byte("\x1b[200~")
	for i := 0; len(b) < n; i++ {
		b = strconv.AppendInt(b, int64(i), 10)
		b = append(b, '\n')
	}
	return append(b, "\x1b[201~"...)
}

// requireSameBytes compares large byte streams and reports the first
// difference instead of both streams.
func requireSameBytes(t *testing.T, got, want []byte) {
	t.Helper()
	if bytes.Equal(got, want) {
		return
	}
	i := 0
	for i < min(len(got), len(want)) && got[i] == want[i] {
		i++
	}
	t.Fatalf("child received %d bytes, want %d; first difference at %d: got %q, want %q",
		len(got), len(want), i, got[i:min(len(got), i+32)], want[i:min(len(want), i+32)])
}
