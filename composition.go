package frame

import (
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

func (f *Frame[T]) compose(dst uv.ScreenBuffer, g geometry, snap Snapshot, force bool) {
	dst.Clear()
	f.paintRegions(dst, g, snap, force)
	if g.child != g.border {
		paintBorder(dst, g.border)
	}
	paintChild(dst, g.child, snap.Terminal)
}

func paintBorder(dst uv.Screen, r uv.Rectangle) {
	b := lipgloss.RoundedBorder()
	put := func(x, y int, text string) { dst.SetCell(x, y, uv.NewCell(ansi.GraphemeWidth, text)) }
	for x := r.Min.X + 1; x < r.Max.X-1; x++ {
		put(x, r.Min.Y, b.Top)
		put(x, r.Max.Y-1, b.Bottom)
	}
	for y := r.Min.Y + 1; y < r.Max.Y-1; y++ {
		put(r.Min.X, y, b.Left)
		put(r.Max.X-1, y, b.Right)
	}
	put(r.Min.X, r.Min.Y, b.TopLeft)
	put(r.Max.X-1, r.Min.Y, b.TopRight)
	put(r.Min.X, r.Max.Y-1, b.BottomLeft)
	put(r.Max.X-1, r.Max.Y-1, b.BottomRight)
}

func paintChild(dst uv.Screen, area uv.Rectangle, term TerminalSnapshot) {
	if term.Size.Cols <= 0 || term.Size.Rows <= 0 {
		return
	}
	// Cells form the backend's owned row-major viewport, including width-zero
	// continuation cells. A shortened snapshot paints its available rows only.
	rows := min(area.Dy(), term.Size.Rows)
	for y := 0; y < rows && y <= len(term.Cells)/term.Size.Cols; y++ {
		start := y * term.Size.Cols
		end := min(len(term.Cells)-start, term.Size.Cols, area.Dx())
		for x := 0; x < end; {
			x += paintCell(dst, &term.Cells[start+x], area.Min.X+x, area.Min.Y+y, area.Max.X)
		}
	}
}

func paintCell(dst uv.Screen, cell *uv.Cell, x, y, maxX int) int {
	if cell == nil || cell.Width <= 0 || cell.Width > maxX-x {
		// Skipping an invalid or partial grapheme guarantees forward progress.
		return 1
	}
	dst.SetCell(x, y, cell)
	return cell.Width
}
