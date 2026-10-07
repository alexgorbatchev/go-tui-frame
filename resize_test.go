package frame

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexgorbatchev/go-tui-frame/internal/emulator"
	"github.com/alexgorbatchev/go-tui-frame/internal/input"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// newProbedSession negotiates with the harness terminal and builds the session
// Run would enter, with the child's native terminal on a separate real PTY.
// The console is restored when the test ends unless the test restored it.
func newProbedSession(t *testing.T, h *terminalHarness) *session[struct{}] {
	t.Helper()
	fd, device, w, err := inspectConsole(h.slave, h.slave)
	if err != nil {
		t.Fatal(err)
	}
	c, err := acquireConsole(h.slave, h.slave, fd, device)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Restoration releases the device. A test that inspects the restored
		// terminal restores the console itself, and Run restores it only once.
		consoleOwners.Lock()
		owned := consoleOwners.devices[device]
		consoleOwners.Unlock()
		if !owned {
			return
		}
		if err := c.restore(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := c.probe(ctx, nil, capabilityTimeout); err != nil {
		t.Fatal(err)
	}
	f := New(exec.Command("sh"), struct{}{})
	g, err := f.checkedLayout(w)
	if err != nil {
		t.Fatal(err)
	}
	em, err := emulator.New(emulator.Options{Size: c.childSize(g)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(em.Close)
	m, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Error(err)
		}
		if err := slave.Close(); err != nil {
			t.Error(err)
		}
	})
	router, err := newInputRouter(em, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(router.close)
	return &session[struct{}]{frame: f, console: c, terminal: em, router: router, framer: c.framer, geometry: g, fd: int(m.Fd()), screen: uv.NewScreenBuffer(g.outer.Dx(), g.outer.Dy())}
}

func TestResizeUpdatesAndClearsPixelOnlyGeometry(t *testing.T) {
	h := newHarness(t)
	s := newProbedSession(t, h)
	for _, tt := range []struct {
		name          string
		x, y          uint16
		width, height uint32
	}{{"changed", 800, 360, 20, 30}, {"unknown", 0, 0, 0, 0}} {
		t.Run(tt.name, func(t *testing.T) {
			if err := pty.Setsize(h.slave, &pty.Winsize{Cols: 40, Rows: 12, X: tt.x, Y: tt.y}); err != nil {
				t.Fatal(err)
			}
			if err := s.resize(); err != nil {
				t.Fatal(err)
			}
			state, err := s.terminal.State()
			if err != nil {
				t.Fatal(err)
			}
			if state.Size.CellWidthPx != tt.width || state.Size.CellHeightPx != tt.height {
				t.Fatalf("pixel-only resize = %#v, want %dx%d", state.Size, tt.width, tt.height)
			}
		})
	}
}

func TestResizeConsumesEveryCellSizeReply(t *testing.T) {
	for _, tt := range []struct {
		name          string
		width, height uint16
		// resized counts the relayouts the replies cause: only the first reply
		// of a measuring terminal changes the cell size the resizes cleared.
		resized int
	}{
		{"measured", harnessCellWidth, harnessCellHeight, 1},
		{"zero", 0, 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newCellHarness(t, 40, 12, tt.width, tt.height)
			s := newProbedSession(t, h)
			// A window drag delivers SIGWINCH faster than the terminal answers,
			// and each winsize without pixels sends another CSI 16 t.
			for _, cols := range []uint16{41, 42} {
				if err := pty.Setsize(h.slave, &pty.Winsize{Cols: cols, Rows: 12}); err != nil {
					t.Fatal(err)
				}
				if err := s.resize(); err != nil {
					t.Fatal(err)
				}
			}
			var (
				mu      sync.Mutex
				resized int
			)
			s.events = newEventDispatcher(eventBit(Resized), sessionCancel(t), func(Event) {
				mu.Lock()
				resized++
				mu.Unlock()
			})
			reply := fmt.Sprintf("\x1b[6;%d;%dt", tt.height, tt.width)
			for _, p := range readOuterReplies(t, s, reply, 2) {
				if err := s.route(p); err != nil {
					t.Fatal(err)
				}
			}
			s.events.close()
			var routed []byte
			for _, p := range s.queue {
				routed = append(routed, p.bytes[p.offset:]...)
			}
			if len(routed) != 0 {
				t.Errorf("child received %q, want no cell-size reply", routed)
			}
			mu.Lock()
			if resized != tt.resized {
				t.Errorf("replies caused %d Resized events, want %d", resized, tt.resized)
			}
			mu.Unlock()
			if c := s.console; c.cellWidth != uint32(tt.width) || c.cellHeight != uint32(tt.height) {
				t.Errorf("measured cells = %dx%d, want %dx%d", c.cellWidth, c.cellHeight, tt.width, tt.height)
			}
			state, err := s.terminal.State()
			if err != nil {
				t.Fatal(err)
			}
			if state.Size.CellWidthPx != uint32(tt.width) || state.Size.CellHeightPx != uint32(tt.height) {
				t.Errorf("child terminal cells = %#v, want %dx%d", state.Size, tt.width, tt.height)
			}
		})
	}
}

// readOuterReplies reads the outer terminal's input until it holds n copies of
// reply and decodes it with the session's framer, as readOuter does.
func readOuterReplies(t *testing.T, s *session[struct{}], reply string, n int) []input.Packet {
	t.Helper()
	var received []byte
	buf := make([]byte, 4096)
	deadline := time.Now().Add(2 * time.Second)
	for strings.Count(string(received), reply) < n {
		wait := time.Until(deadline)
		if wait <= 0 {
			t.Fatalf("outer terminal sent %q, want %d copies of %q", received, n, reply)
		}
		fds := []unix.PollFd{{Fd: int32(s.console.fd), Events: unix.POLLIN}}
		if _, err := unix.Poll(fds, int(wait.Milliseconds())+1); err != nil && !errors.Is(err, unix.EINTR) {
			t.Fatal(err)
		}
		if fds[0].Revents&unix.POLLIN == 0 {
			continue
		}
		read, err := unix.Read(s.console.fd, buf)
		if err != nil && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			t.Fatal(err)
		}
		received = append(received, buf[:max(read, 0)]...)
	}
	packets, err := s.framer.Feed(received)
	if err != nil {
		t.Fatal(err)
	}
	return packets
}

// A window resized while Run starts, after it read the outer size, must still
// reach the session: the first loop iteration applies the new size.
func TestRunAppliesResizeDuringStartup(t *testing.T) {
	h := newHarness(t)
	// The harness answers neither DA1 nor the modifyOtherKeys query, so the
	// probe runs to this deadline and the resize below lands during startup.
	// The test checks that the signal was sent before the child's start, which
	// precedes the loop, instead of relying on that.
	h.withholdPrimaryDeviceAttributes()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	const probeTimeout = time.Second
	want := Size{Cols: 32, Rows: 10}
	var resize sync.Once
	signaled, started, resized := make(chan time.Time, 1), make(chan time.Time, 1), make(chan Snapshot, 1)
	kinds := []EventKind{OuterInput, Started, Resized}
	app := New(childCommand(t), struct{}{}).Terminal(h.slave, h.slave).ObserveEvents(kinds, func(e Event) {
		switch {
		case e.Kind == OuterInput && e.Origin == "terminal-probe":
			resize.Do(func() {
				// The harness PTY is not this process's controlling terminal,
				// so the window manager's SIGWINCH is sent explicitly.
				if err := pty.Setsize(h.slave, &pty.Winsize{Cols: uint16(want.Cols), Rows: uint16(want.Rows), X: uint16(want.Cols * harnessCellWidth), Y: uint16(want.Rows * harnessCellHeight)}); err != nil {
					t.Error(err)
				}
				if err := unix.Kill(os.Getpid(), unix.SIGWINCH); err != nil {
					t.Error(err)
				}
				signaled <- time.Now()
			})
		case e.Kind == Started:
			started <- e.Snapshot.Child.StartedAt
		case e.Kind == Resized:
			select {
			case resized <- *e.Snapshot:
			default:
			}
		}
	})
	app.probeTimeout = probeTimeout
	done := startRun(ctx, app)
	select {
	case childStarted := <-started:
		// The observer handles events in order, so the probe event that sent
		// the signal was handled before Started.
		select {
		case sent := <-signaled:
			if !sent.Before(childStarted) {
				t.Fatalf("SIGWINCH sent at %v, after the child started at %v: the resize did not land during startup", sent, childStarted)
			}
		default:
			t.Fatal("Started arrived before the probe input that sends SIGWINCH")
		}
	case got := <-done:
		t.Fatalf("Run ended before Started: %#v, %v", got.result, got.err)
	case <-ctx.Done():
		t.Fatal("Started was not observed", ctx.Err())
	}
	select {
	case s := <-resized:
		if s.Outer != want || s.Viewport != want {
			t.Errorf("Resized outer = %v, viewport = %v, want %v", s.Outer, s.Viewport, want)
		}
	case <-time.After(3 * time.Second):
		t.Error("a resize during startup produced no Resized event")
	}
	if _, err := unix.Write(h.fd, []byte("q")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Run = %#v, %v", got.result, got.err)
		}
	case <-ctx.Done():
		t.Fatal("Run did not finish", ctx.Err())
	}
}

// stateFieldDiff names each field of got that is not deeply equal to want's.
func stateFieldDiff(got, want emulator.State) []string {
	g, w := reflect.ValueOf(got), reflect.ValueOf(want)
	var diff []string
	for i := range g.NumField() {
		if !reflect.DeepEqual(g.Field(i).Interface(), w.Field(i).Interface()) {
			diff = append(diff, fmt.Sprintf("%s = %v, want %v", g.Type().Field(i).Name, g.Field(i), w.Field(i)))
		}
	}
	return diff
}

// Resize and the render-hold deadline change the child's native terminal
// without child output, so no child read refreshes the routing state; the
// refresh that follows them must leave exactly what InputState reports.
func TestNativeMutationRefreshesRoutingState(t *testing.T) {
	type modes = map[ansi.DECMode]ansi.ModeSetting
	for _, tt := range []struct {
		name    string
		reports modes
		child   string
		mutate  func(t *testing.T, s *session[string])
		// outer is routed after the mutation; the child must receive routed.
		outer, routed string
		held          bool
	}{
		{
			// The outer terminal keeps pixel reports on and the child asks for
			// cells, so the routed cell depends on the resized cell geometry.
			name: "resize", reports: modes{1006: ansi.ModeReset, 1016: ansi.ModePermanentlySet},
			child: "\x1b[?1000;1006h",
			mutate: func(t *testing.T, s *session[string]) {
				s.console.cellWidth, s.console.cellHeight = 12, 24
				if err := s.applyGeometry(s.geometry); err != nil {
					t.Fatal(err)
				}
			},
			outer: "\x1b[<0;61;91M", routed: "\x1b[<0;6;4M",
		},
		{
			name: "hold deadline", child: "\x1b[?1004h\x1b[?2026h",
			mutate: func(t *testing.T, s *session[string]) {
				s.holdDeadline = time.Now().Add(-time.Second)
				if err := s.deadlines(); err != nil {
					t.Fatal(err)
				}
			},
			outer: "\x1b[I", routed: "\x1b[I",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, _, slave := newRepaintSession(t, ansi.ModeReset, nil)
			c := s.console
			c.cellWidth, c.cellHeight = harnessCellWidth, harnessCellHeight
			router, err := newInputRouter(s.terminal, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(router.close)
			s.router = router
			reportModes(t, c, tt.reports)
			if err := s.applyGeometry(s.geometry); err != nil {
				t.Fatal(err)
			}
			readRepaintChunk(t, s, slave, tt.child)
			tt.mutate(t, s)
			var want emulator.State
			if err := s.terminal.InputState(&want); err != nil {
				t.Fatal(err)
			}
			if diff := stateFieldDiff(s.inputState, want); len(diff) != 0 {
				t.Errorf("routing state after %s differs from InputState:\n%s", tt.name, strings.Join(diff, "\n"))
			}
			if s.inputState.Held != tt.held {
				t.Errorf("routing state held = %v, want %v", s.inputState.Held, tt.held)
			}
			queued := len(s.queue)
			packets, err := input.New().Feed([]byte(tt.outer))
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range packets {
				if err := s.route(p); err != nil {
					t.Fatalf("routing %q failed: %v", tt.outer, err)
				}
			}
			var routed []byte
			for _, p := range s.queue[queued:] {
				routed = append(routed, p.bytes[p.offset:]...)
			}
			if string(routed) != tt.routed {
				t.Errorf("child received %q for outer %q, want %q", routed, tt.outer, tt.routed)
			}
		})
	}
}
