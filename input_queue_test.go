package frame

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
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
// session. Mouse gestures, which the frame re-encodes for the child, check
// that routing expansion stays bounded.
func TestOuterInputWaitsForBusyChild(t *testing.T) {
	const gestures = 8_000
	paste := bracketedPaste(5 * inputQueueLimit / 2)
	for _, tt := range []struct {
		name, childModes string
		input, want      []byte
	}{
		{name: "bracketed paste", childModes: "\x1b[?2004h", input: paste, want: paste},
		// Each 36-byte gesture turns into 144 child-input bytes.
		{name: "mouse gestures", childModes: gestureModes, input: bytes.Repeat([]byte(x10Gesture), gestures), want: bytes.Repeat([]byte(sgrGesture), gestures)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, outer, child := newQueueSession(t, tt.childModes, io.Discard)
			_, written := feedOuter(t, outer, tt.input)
			deadline := time.Now().Add(30 * time.Second)
			wakeAt(t, s, deadline)
			buf := make([]byte, readBufferSize)
			pauseOuterInput(t, s, buf, deadline)
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
	s, outer, _ := newQueueSession(t, "\x1b[?2004h", io.Discard)
	stop, _ := feedOuter(t, outer, bracketedPaste(2*inputQueueLimit))
	deadline := time.Now().Add(30 * time.Second)
	wakeAt(t, s, deadline)
	buf := make([]byte, readBufferSize)
	pauseOuterInput(t, s, buf, deadline)
	stop()
	const limit = time.Second
	closed := make(chan error, 1)
	start := time.Now()
	go func() {
		time.Sleep(50 * time.Millisecond)
		closed <- outer.Close()
	}()
	wakeAt(t, s, start.Add(3*limit))
	wakes := 0
	for {
		paused, disconnected := stepUninterrupted(t, s, buf)
		wakes++
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
	// Starting the watch may report the input already waiting once; otherwise
	// the paused loop sleeps until the hangup.
	if wakes > 2 {
		t.Fatalf("paused loop woke %d times before the hangup, want at most 2", wakes)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

// An Escape that ends the read which paused outer input cannot time out into a
// bare Escape while the bytes completing its sequence wait unread.
func TestPausedOuterInputKeepsEscapeSequences(t *testing.T) {
	// A timed-out Escape would reach capture as Escape, "[" and "A" instead of
	// the Up arrow "\x1b[A" whose first byte it is. Mouse gestures, which
	// need more room than the full queue has, hold it behind them.
	s, outer, child := newQueueSession(t, gestureModes, io.Discard)
	var keys []string
	s.router.filter.handler = func(in Input) Disposition {
		keys = append(keys, in.Key.Key().Keystroke())
		return Pass
	}
	gestures, routed := heldGestures()
	want := append(fillChild(t, s, inputQueueLimit-10), routed...)
	want = append(want, "ab\x1b[A"...)
	// One small write reaches the session in one read, on Linux as one
	// line-discipline buffer.
	if _, err := outer.Write(append(gestures, "ab\x1b"...)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	wakeAt(t, s, deadline)
	buf := make([]byte, readBufferSize)
	if paused, _ := step(t, s, buf); !paused || !s.framer.Pending() {
		t.Fatalf("after the read: paused=%v, pending Escape=%v; want both while the child reads nothing", paused, s.framer.Pending())
	}
	// Let the Escape deadline pass, then wake the loop as a resize or region
	// invalidation would. The expired deadline must neither spin the paused
	// loop nor flush the Escape.
	time.Sleep(3 * escapeTimeout)
	if timeout := s.pollTimeout(); timeout != -1 {
		t.Fatalf("paused loop polls with a %d ms timeout, want none", timeout)
	}
	s.wakeLoop()
	if paused, _ := step(t, s, buf); !paused || !s.framer.Pending() {
		t.Fatalf("after the Escape deadline: paused=%v, pending Escape=%v; want both", paused, s.framer.Pending())
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
			if !slices.Equal(keys, []string{"a", "b", "up"}) {
				t.Fatalf("capture saw keys %q, want a, b and up", keys)
			}
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("child did not receive its input; queued %d", s.queuedBytes)
		}
	}
}

// A terminal reply held behind a full child queue changes outer modes when it
// is routed. Those mode bytes reach the terminal at once, as after an outer
// read, instead of waiting for the next repaint.
func TestReleasedReplyReachesTerminalWithoutRepaint(t *testing.T) {
	var out bytes.Buffer
	// The child reports focus. The terminal's DECRPM reply makes its mode 1004
	// switchable, so routing the reply mirrors the child's mode there. Mouse
	// gestures, which need more room than the full queue has, hold the reply
	// behind them.
	s, outer, child := newQueueSession(t, "\x1b[?1004h"+gestureModes, &out)
	s.console.pending = map[ansi.DECMode]bool{1004: true}
	gestures, routed := heldGestures()
	want := append(fillChild(t, s, inputQueueLimit-10), routed...)
	if _, err := outer.Write(append(gestures, "\x1b[?1004;2$y"...)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	wakeAt(t, s, deadline)
	buf := make([]byte, readBufferSize)
	if paused, _ := step(t, s, buf); !paused || !s.console.pending[1004] {
		t.Fatalf("paused=%v, reply pending=%v; want the reply held while the child reads nothing", paused, s.console.pending[1004])
	}
	written := out.Len()
	collectChild(t, s, child, len(want))
	for s.console.pending[1004] {
		step(t, s, buf)
		if time.Now().After(deadline) {
			t.Fatalf("held reply was not routed; queued %d", s.queuedBytes)
		}
	}
	if sent := out.Bytes()[written:]; !bytes.Contains(sent, []byte("\x1b[?1004h")) {
		t.Fatalf("routing the held reply wrote %q to the terminal, want focus reporting enabled", sent)
	}
}

// More than a queue of input arriving while the frame starts reaches the child
// once it reads, instead of ending the session as it is routed. The session
// starts with outer reads paused, and Run closes every descriptor it opened.
func TestRunHoldsStartupInputBeyondTheQueue(t *testing.T) {
	h := newHarness(t)
	paste := bracketedPaste(inputQueueLimit + readBufferSize)
	// The terminal answers the frame's capability queries only after the
	// paste, so the startup probe must read all of it before the loop starts.
	// It also answers the queries its emulator ignores, so the probe ends at
	// its last reply rather than at a deadline a loaded host could miss.
	h.precede(paste)
	h.answer(ansi.QueryModifyOtherKeys, "\x1b[>4;0m")
	h.answer(colorSchemeQuery, "\x1b[?997;1n")
	dir, err := os.MkdirTemp(".tmp", "startup-input-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	count := dir + "/count"
	cmd := childCommand(t)
	cmd.Env = append(cmd.Env, "FRAME_TEST_MODE=count", "FRAME_TEST_COUNT_FILE="+count)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	app := New(cmd, struct{}{}).Terminal(h.slave, h.slave)
	app.probeTimeout = 20 * time.Second
	// The first signal subscription in a process makes Darwin's Go runtime open
	// a signal pipe it keeps for good. Run subscribes to SIGWINCH, so subscribe
	// first: the baseline then holds only descriptors Run must leave as found.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGWINCH)
	signal.Stop(signals)
	opened := openDescriptors(t)
	done := startRun(ctx, app)
	for !strings.Contains(h.text(), "counting") {
		select {
		case got := <-done:
			t.Fatalf("Run ended before the child read its input: %#v, %v", got.result, got.err)
		case <-ctx.Done():
			t.Fatal("child did not start", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, err := unix.Write(h.fd, []byte("q")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil || got.result.ProcessState == nil || !got.result.ProcessState.Success() {
			t.Fatalf("Run = %#v, %v", got.result, got.err)
		}
	case <-ctx.Done():
		t.Fatal("Run did not finish", ctx.Err())
	}
	if leaked := openDescriptors(t) - opened; leaked != 0 {
		t.Fatalf("Run left %d more descriptors open than before it started", leaked)
	}
	received, err := os.ReadFile(count)
	if err != nil {
		t.Fatal(err)
	}
	// The child does not request bracketed paste, so its markers are dropped.
	if want := strconv.Itoa(len(paste) - len("\x1b[200~\x1b[201~")); string(received) != want {
		t.Fatalf("child received %s pasted bytes, want %s", received, want)
	}
}

// newQueueSession builds a session between a real outer terminal PTY, whose
// master the test writes, and a child PTY whose slave nothing reads until the
// test collects it. childModes sets the child's native input modes; out
// receives what the session writes to the terminal.
func newQueueSession(t *testing.T, childModes string, out io.Writer) (s *session[struct{}], outer, child *os.File) {
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
	// x10Gesture clicks at column 200, row 100.
	g, err := f.layout(Size{Cols: 200, Rows: 100})
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
	c := &console{input: outerSlave, fd: int(outerSlave.Fd()), framer: input.New(), renderer: uv.NewTerminalRenderer(out, []string{"TERM=xterm-256color"}),
		entry: make(map[ansi.DECMode]ansi.ModeSetting), applied: make(map[ansi.DECMode]bool)}
	s = &session[struct{}]{frame: f, console: c, terminal: em, router: router, geometry: g, screen: c.outerScreen(g), framer: c.framer, fd: childFD}
	if err := s.openWake(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.closeWake(); err != nil {
			t.Error(err)
		}
		if err := s.hangup.close(); err != nil {
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
// one poll, and every ready descriptor. It reports whether the next poll
// watches the outer terminal without requesting its input, and whether the
// terminal disconnected. Any session error or queue overrun fails the test.
func step(t *testing.T, s *session[struct{}], buf []byte) (paused, disconnected bool) {
	t.Helper()
	return stepWith(t, s, func() (bool, error) { return s.await(buf, nil, nil) })
}

// stepUninterrupted repeats step's iteration while a signal interrupts its
// poll, so each call returns one wake of the loop. await returns an
// interrupted poll as an empty iteration, which the loop repeats at once, and
// Go's asynchronous preemption signal, SIGURG, can interrupt a blocked poll at
// any time. Otherwise it waits as await does.
func stepUninterrupted(t *testing.T, s *session[struct{}], buf []byte) (paused, disconnected bool) {
	t.Helper()
	for {
		interrupted := false
		paused, disconnected = stepWith(t, s, func() (bool, error) {
			hungUp, err := s.hangup.watch(s.console.fd, s.watchesHangup(nil))
			if err != nil || hungUp {
				return hungUp, err
			}
			fds := s.pollFDs(nil)
			_, err = unix.Poll(fds, s.pollTimeout())
			if errors.Is(err, unix.EINTR) {
				interrupted = true
				return false, nil
			}
			if err != nil {
				return false, err
			}
			return s.processReady(buf, fds, nil, true)
		})
		if !interrupted {
			return paused, disconnected
		}
	}
}

// stepWith runs step's iteration with wait in place of await.
func stepWith(t *testing.T, s *session[struct{}], wait func() (bool, error)) (paused, disconnected bool) {
	t.Helper()
	if err := s.deadlines(); err != nil {
		t.Fatal(err)
	}
	disconnected, err := wait()
	if err != nil {
		t.Fatal(err)
	}
	if s.queuedBytes > inputQueueLimit {
		t.Fatalf("queued %d child-input bytes, over the %d-byte limit", s.queuedBytes, inputQueueLimit)
	}
	fds := s.pollFDs(nil)
	paused = fds[1].Fd == int32(s.console.fd) && fds[1].Events&unix.POLLIN == 0
	return paused, disconnected
}

// pauseOuterInput steps the session until it stops reading the terminal, then
// waits for the terminal to hold input the session left unread.
func pauseOuterInput(t *testing.T, s *session[struct{}], buf []byte, deadline time.Time) {
	t.Helper()
	for {
		if paused, _ := step(t, s, buf); paused {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("outer reads never paused; queued %d", s.queuedBytes)
		}
	}
	if !inputWaiting(t, s.console.fd, time.Until(deadline)) {
		t.Fatal("the terminal holds no unread input while outer reads are paused")
	}
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

// gestureModes requests the mouse tracking and SGR reports that x10Gesture
// routes to.
const gestureModes = "\x1b[?1000;1006h"

// x10Gesture is a 36-byte mouse gesture from an outer terminal without SGR
// reports: presses of the left, middle, right, back and forward buttons with
// Shift, Alt and Ctrl at column 200, row 100, then the X10 release that names
// no button. sgrGesture is the 144 bytes it routes to for a gestureModes
// child: each press, then a release of each pressed button in button order.
const (
	x10Gesture = "\x1b[M\x3c\xe8\x84\x1b[M\x3d\xe8\x84\x1b[M\x3e\xe8\x84\x1b[M\xbc\xe8\x84\x1b[M\xbd\xe8\x84\x1b[M\x3f\xe8\x84"
	sgrGesture = "\x1b[<28;200;100M\x1b[<29;200;100M\x1b[<30;200;100M\x1b[<156;200;100M\x1b[<157;200;100M" +
		"\x1b[<28;200;100m\x1b[<29;200;100m\x1b[<30;200;100m\x1b[<156;200;100m\x1b[<157;200;100m"
)

// heldGestures returns outer mouse gestures and the bytes they route to. Each
// mouse report needs maxRoutedExpansion times its length of queue room, more
// than the 10 bytes fillChild(t, s, inputQueueLimit-10) leaves, so a session
// holds every gesture and the input behind them while the child reads
// nothing. Their 144 input bytes, with a short tail, fit in the 1022 bytes
// Darwin's PTY accepts in one write before a reader drains it, so the tail
// reaches the session in the same read. The child must request gestureModes.
func heldGestures() (gestures, routed []byte) {
	const n = 4
	return bytes.Repeat([]byte(x10Gesture), n), bytes.Repeat([]byte(sgrGesture), n)
}

// childSettleTime is how long fillChild waits for a full child PTY to free
// room before treating it as full.
const childSettleTime = 250 * time.Millisecond

// fillChild queues n filler bytes and writes them until the child's PTY takes
// no more, then tops the queue up to n bytes again. It returns every filler
// byte the child will read. Linux only queues a PTY write; the kernel later
// moves it into the slave's line-discipline buffer, which frees room after a
// write has failed with EAGAIN. fillChild therefore waits for the PTY to
// become writable again and keeps writing until it stays full for
// childSettleTime or reports room twice without taking a byte.
func fillChild(t *testing.T, s *session[struct{}], n int) []byte {
	t.Helper()
	fill := bytes.Repeat([]byte("x"), n)
	if err := s.enqueue(fill, "user-input"); err != nil {
		t.Fatal(err)
	}
	for stalled := 0; stalled < 2; {
		queued := s.queuedBytes
		if err := s.writeInput(); err != nil {
			t.Fatal(err)
		}
		if s.queuedBytes < queued {
			stalled = 0
			continue
		}
		fds := []unix.PollFd{{Fd: int32(s.fd), Events: unix.POLLOUT}}
		ready, err := unix.Poll(fds, int(childSettleTime.Milliseconds()))
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if ready == 0 {
			break
		}
		stalled++
	}
	top := bytes.Repeat([]byte("x"), n-s.queuedBytes)
	if err := s.enqueue(top, "user-input"); err != nil {
		t.Fatal(err)
	}
	return append(fill, top...)
}

// openDescriptors counts the file descriptors this process has open, kqueues
// included, which Darwin's /dev/fd does not list. The kernel hands out the
// lowest free descriptor, so a test process's descriptors lie far below the
// scanned range.
func openDescriptors(t *testing.T) int {
	t.Helper()
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	open := 0
	for fd := range min(limit.Cur, 1<<16) {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == nil {
			open++
		}
	}
	return open
}

// inputWaiting reports whether bytes wait unread in the terminal's input queue
// within timeout. It reads nothing.
func inputWaiting(t *testing.T, fd int, timeout time.Duration) bool {
	t.Helper()
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for {
		_, err := unix.Poll(fds, int(timeout.Milliseconds()))
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		return fds[0].Revents&unix.POLLIN != 0
	}
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
