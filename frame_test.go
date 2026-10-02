package frame

import (
	"errors"
	"image/color"
	"math"
	"os/exec"
	"sync"
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
)

func TestFrameLayout(t *testing.T) {
	tests := []struct {
		name string
		size Size
		want uv.Rectangle
		fail bool
	}{
		{"roomy", Size{Cols: 40, Rows: 16}, uv.Rect(3, 4, 32, 9), false},
		{"resized", Size{Cols: 24, Rows: 12}, uv.Rect(3, 4, 16, 5), false},
		{"too narrow", Size{Cols: 8, Rows: 16}, uv.Rectangle{}, true},
		{"too short", Size{Cols: 40, Rows: 7}, uv.Rectangle{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			draw := func(DrawContext[string]) {}
			f := New(exec.Command("sh"), "ready").Header(3, draw).Footer(2, draw).
				Left(2, draw).Right(4, draw).Border(true)
			g, err := f.layout(tt.size)
			if tt.fail {
				if !errors.Is(err, ErrViewportTooSmall) {
					t.Fatalf("layout error = %v, want viewport error", err)
				}
				return
			}
			if err != nil || g.child != tt.want {
				t.Fatalf("child = %v, error = %v, want %v", g.child, err, tt.want)
			}
		})
	}
}

func TestPayloadUpdatesAreRegionLocalAndCoalesce(t *testing.T) {
	var gotHeader, gotFooter string
	f := New(exec.Command("sh"), "initial").
		Header(2, func(ctx DrawContext[string]) { gotHeader = ctx.Data }).
		Footer(1, func(ctx DrawContext[string]) { gotFooter = ctx.Data })
	for _, next := range []string{"first", "second", "latest"} {
		if err := f.InvalidateHeader(next); err != nil {
			t.Fatal(err)
		}
	}
	g, err := f.layout(Size{Cols: 20, Rows: 8})
	if err != nil {
		t.Fatal(err)
	}
	f.paintRegions(uv.NewScreenBuffer(20, 8), g, Snapshot{}, true)
	if gotHeader != "latest" || gotFooter != "initial" {
		t.Fatalf("header = %q, footer = %q", gotHeader, gotFooter)
	}
	if f.cmd.Process != nil {
		t.Fatal("configuration or drawing started the command")
	}
}

func TestInvalidationDuringDrawDoesNotBlockOrLoseUpdate(t *testing.T) {
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	var once sync.Once
	var got string
	f := New(exec.Command("sh"), "initial").Header(1, func(ctx DrawContext[string]) {
		once.Do(func() { close(entered); <-release })
		got = ctx.Data
	})
	g, err := f.layout(Size{Cols: 20, Rows: 8})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		f.paintRegions(uv.NewScreenBuffer(20, 8), g, Snapshot{}, true)
		close(done)
	}()
	<-entered
	updated := make(chan error, 1)
	go func() { updated <- f.InvalidateHeader("while drawing") }()
	select {
	case err := <-updated:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("invalidation waited for drawing callback")
	}
	close(release)
	<-done
	f.paintRegions(uv.NewScreenBuffer(20, 8), g, Snapshot{}, false)
	if got != "while drawing" {
		t.Fatalf("latest payload lost: %q", got)
	}
}

func TestInvalidateAbsentAndClosedRegions(t *testing.T) {
	f := New(exec.Command("sh"), "initial").Header(1, func(DrawContext[string]) {})
	if err := f.InvalidateFooter("missing"); !errors.Is(err, ErrRegionNotConfigured) {
		t.Fatalf("absent region error = %v", err)
	}
	f.close()
	if err := f.InvalidateHeader("late"); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("closed session error = %v", err)
	}
}

func TestNativeScreenIsClippedToRegion(t *testing.T) {
	f := New(exec.Command("sh"), "red").Header(2, func(ctx DrawContext[string]) {
		bounds := ctx.View.Bounds()
		cell := uv.Cell{Content: " ", Width: 1, Style: uv.Style{Bg: color.RGBA{R: 185, G: 28, B: 28, A: 255}}}
		for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
			for x := bounds.Min.X; x < bounds.Max.X; x++ {
				ctx.View.SetCell(x, y, &cell)
			}
		}
	})
	g, err := f.layout(Size{Cols: 12, Rows: 6})
	if err != nil {
		t.Fatal(err)
	}
	buf := uv.NewScreenBuffer(12, 6)
	child := uv.Cell{Content: "X", Width: 1}
	buf.SetCell(0, 2, &child)
	f.paintRegions(buf, g, Snapshot{}, true)
	if cell := buf.CellAt(0, 2); cell == nil || cell.Content != "X" {
		t.Fatal("header erased child content")
	}
	if cell := buf.CellAt(11, 1); cell == nil || cell.Style.Bg == nil {
		t.Fatal("header background did not fill its allocation")
	}
}

func TestLayoutRejectsOverflowingReservations(t *testing.T) {
	for _, size := range []int{math.MaxInt, math.MaxInt - 1} {
		f := New(exec.Command("sh"), "ready").Header(size, func(DrawContext[string]) {}).
			Footer(size, func(DrawContext[string]) {})
		if _, err := f.layout(Size{Cols: 80, Rows: 24}); !errors.Is(err, ErrViewportTooSmall) {
			t.Fatalf("accepted overflowing reservations %d: %v", size, err)
		}
	}
}

func TestInvalidCanvasCellWidthsCannotHangClipping(t *testing.T) {
	for _, width := range []int{-1, math.MinInt, math.MaxInt} {
		canvas := uv.NewScreenBuffer(1, 1)
		canvas.SetCell(0, 0, &uv.Cell{Content: "x", Width: width})
		buf := uv.NewScreenBuffer(1, 1)
		drawClipped(buf, canvas, uv.Rect(0, 0, 1, 1))
		if cell := buf.CellAt(0, 0); cell != nil && cell.Content == "x" {
			t.Fatalf("invalid width %d reached destination", width)
		}
	}
}
