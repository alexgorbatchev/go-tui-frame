package frame

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/alexgorbatchev/go-tui-frame/internal/emulator"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/creack/pty"
)

func TestResizeUpdatesAndClearsPixelOnlyGeometry(t *testing.T) {
	h := newHarness(t)
	fd, device, w, err := inspectConsole(h.slave, h.slave)
	if err != nil {
		t.Fatal(err)
	}
	c, err := acquireConsole(h.slave, h.slave, fd, device)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
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
	s := &session[struct{}]{frame: f, console: c, terminal: em, geometry: g, fd: int(m.Fd()), screen: uv.NewScreenBuffer(g.outer.Dx(), g.outer.Dy())}
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
			state, err := em.State()
			if err != nil {
				t.Fatal(err)
			}
			if state.Size.CellWidthPx != tt.width || state.Size.CellHeightPx != tt.height {
				t.Fatalf("pixel-only resize = %#v, want %dx%d", state.Size, tt.width, tt.height)
			}
		})
	}
}
