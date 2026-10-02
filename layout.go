package frame

import (
	"fmt"
	"image"
	"slices"

	"github.com/alexgorbatchev/go-tui-frame/internal/emulator"
	"github.com/alexgorbatchev/go-tui-frame/internal/process"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

type geometry struct {
	outer   uv.Rectangle
	child   uv.Rectangle
	border  uv.Rectangle
	regions [edgeCount]uv.Rectangle
}

func (f *Frame[T]) layout(size Size) (geometry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := geometry{outer: uv.Rect(0, 0, size.Cols, size.Rows)}
	top, bottom := f.regions[header].size, f.regions[footer].size
	lhs, rhs := f.regions[left].size, f.regions[right].size
	margin := 0
	if f.border {
		margin = 1
	}
	// Subtract one reservation at a time: adding arbitrary positive sizes can
	// overflow int and make an impossible layout appear to fit.
	w, h := size.Cols, size.Rows
	for _, cols := range []int{lhs, rhs, 2 * margin} {
		if cols >= w {
			return g, fmt.Errorf("%w: outer size %dx%d", ErrViewportTooSmall, size.Cols, size.Rows)
		}
		w -= cols
	}
	for _, rows := range []int{top, bottom, 2 * margin} {
		if rows >= h {
			return g, fmt.Errorf("%w: outer size %dx%d", ErrViewportTooSmall, size.Cols, size.Rows)
		}
		h -= rows
	}
	if w <= 0 || h <= 0 {
		return g, fmt.Errorf("%w: outer size %dx%d", ErrViewportTooSmall, size.Cols, size.Rows)
	}
	g.regions[header] = uv.Rect(0, 0, size.Cols, top)
	g.regions[footer] = uv.Rect(0, size.Rows-bottom, size.Cols, bottom)
	g.regions[left] = uv.Rect(0, top, lhs, size.Rows-top-bottom)
	g.regions[right] = uv.Rect(size.Cols-rhs, top, rhs, size.Rows-top-bottom)
	g.border = uv.Rect(lhs, top, size.Cols-lhs-rhs, size.Rows-top-bottom)
	g.child = g.border.Inset(margin)
	return g, nil
}

// paintRegions snapshots payloads under the state lock, then draws outside it.
// Clearing dirty before invoking callbacks retains invalidations racing a draw.
func (f *Frame[T]) paintRegions(dst uv.Screen, g geometry, snap Snapshot, force bool) {
	for e := range edgeCount {
		area := g.regions[e]
		f.mu.Lock()
		r := &f.regions[e]
		draw, data := r.draw, r.data
		paint := draw != nil && (force || r.dirty || r.canvas == nil ||
			r.canvas.Width() != area.Dx() || r.canvas.Height() != area.Dy())
		if paint {
			r.dirty = false
		}
		canvas := r.canvas
		f.mu.Unlock()
		if draw == nil || area.Empty() {
			continue
		}
		if paint {
			buf := uv.NewScreenBuffer(area.Dx(), area.Dy())
			buf.Method = ansi.GraphemeWidth
			canvas = &buf
			draw(DrawContext[T]{Term: cloneSnapshot(snap), View: canvas, Data: data})
			f.mu.Lock()
			f.regions[e].canvas = canvas
			f.mu.Unlock()
		}
		drawClipped(dst, canvas, area)
	}
}

func drawClipped(dst uv.Screen, src uv.Screen, area image.Rectangle) {
	limit := area.Intersect(dst.Bounds())
	for y := limit.Min.Y; y < limit.Max.Y; y++ {
		for x := limit.Min.X; x < limit.Max.X; {
			cell := src.CellAt(x-area.Min.X, y-area.Min.Y)
			x += paintCell(dst, cell, x, y, limit.Max.X)
		}
	}
}

func cloneSnapshot(s Snapshot) Snapshot {
	s.Child.Args = slices.Clone(s.Child.Args)
	s.Child.Environment = slices.Clone(s.Child.Environment)
	s.Terminal.Cells = slices.Clone(s.Terminal.Cells)
	s.Terminal.Native = emulator.CloneState(s.Terminal.Native)
	s.Child.OperatingSystem = process.Clone(s.Child.OperatingSystem)
	s.Child.SessionProcesses = slices.Clone(s.Child.SessionProcesses)
	for i := range s.Child.SessionProcesses {
		s.Child.SessionProcesses[i] = process.Clone(s.Child.SessionProcesses[i])
	}
	if s.Child.PTY.Window != nil {
		s.Child.PTY.Window = new(*s.Child.PTY.Window)
	}
	if s.Child.PTY.Settings != nil {
		s.Child.PTY.Settings = new(*s.Child.PTY.Settings)
	}
	return s
}
