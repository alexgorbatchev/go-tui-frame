package frame

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexgorbatchev/go-tui-frame/internal/emulator"
	"github.com/alexgorbatchev/go-tui-frame/internal/input"
	uv "github.com/charmbracelet/ultraviolet"
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
	if _, err := c.probe(ctx, nil); err != nil {
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
