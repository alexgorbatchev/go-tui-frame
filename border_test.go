package frame

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

func TestSetBorderUpdatesLayoutBeforeAndDuringRun(t *testing.T) {
	for _, started := range []bool{false, true} {
		f := New(exec.Command("sh"), struct{}{}).Header(1, func(DrawContext[struct{}]) {}).Border(true)
		if started {
			if err := f.begin(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		for _, enabled := range []bool{false, true, false} {
			if err := f.SetBorder(enabled); err != nil {
				t.Fatal(err)
			}
			g, err := f.layout(Size{Cols: 20, Rows: 8})
			want := uv.Rect(0, 1, 20, 7)
			if enabled {
				want = uv.Rect(1, 2, 18, 5)
			}
			if err != nil || g.child != want || f.configurationError() != nil {
				t.Fatalf("started=%v border=%v: layout=%v, err=%v, config=%v", started, enabled, g.child, err, f.configurationError())
			}
		}
		f.close()
		if err := f.SetBorder(true); !errors.Is(err, ErrSessionClosed) {
			t.Fatalf("closed SetBorder: %v", err)
		}
	}
}

func TestRunBorderChangesResizeNativeChildAndPTY(t *testing.T) {
	h := newHarness(t)
	// This native terminal reports pixels by query, while the OS winsize has
	// cells only. A border-only change must retain those measured cell pixels.
	if err := pty.Setsize(h.slave, &pty.Winsize{Cols: 40, Rows: 12}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	snapshots := make(chan Snapshot, 64)
	app := New(childCommand(t), struct{}{}).Terminal(h.slave, h.slave).
		Header(1, func(c DrawContext[struct{}]) {
			select {
			case snapshots <- c.Term:
			default:
			}
		}).Footer(1, func(DrawContext[struct{}]) {}).Border(true)
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_, err := app.Run(ctx)
		done <- err
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Error("border session did not stop")
		}
	})
	awaitText(t, h, "child")
	for _, tt := range []struct {
		on   bool
		size Size
	}{{false, Size{Cols: 40, Rows: 10}}, {true, Size{Cols: 38, Rows: 8}}} {
		if err := app.SetBorder(tt.on); err != nil {
			t.Fatal(err)
		}
		s := waitBorderSnapshot(t, ctx, snapshots, tt.size)
		native := s.Terminal.Native.Size
		w := s.Child.PTY.Window
		if native.Cols != tt.size.Cols || native.Rows != tt.size.Rows || native.CellWidthPx != 10 || native.CellHeightPx != 20 || w == nil || int(w.Col) != tt.size.Cols || int(w.Row) != tt.size.Rows || int(w.Xpixel) != tt.size.Cols*10 || int(w.Ypixel) != tt.size.Rows*20 {
			t.Fatalf("border=%v native=%#v, PTY=%#v", tt.on, native, w)
		}
	}
	if _, err := unix.Write(h.fd, []byte("q")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func waitBorderSnapshot(t *testing.T, ctx context.Context, snapshots <-chan Snapshot, size Size) Snapshot {
	t.Helper()
	for {
		select {
		case s := <-snapshots:
			if s.Viewport == size {
				return s
			}
		case <-ctx.Done():
			t.Fatal("border update did not resize child", ctx.Err())
		}
	}
}
