package frame

import (
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

const (
	borderHorizontal  = "─"
	borderVertical    = "│"
	borderTopLeft     = "╭"
	borderTopRight    = "╮"
	borderBottomLeft  = "╰"
	borderBottomRight = "╯"
)

type repaintDamage struct {
	full, regions bool
	rows          []bool
}

func screenChanged(screen uv.ScreenBuffer) bool {
	// UV marks consumed lines with (-1, -1); TouchedLines counts those
	// entries too. Check the ranges before asking its renderer to diff.
	for _, line := range screen.Touched {
		if line != nil && (line.FirstCell != -1 || line.LastCell != -1) {
			return true
		}
	}
	return false
}

func (f *Frame[T]) compose(dst *uv.ScreenBuffer, g geometry, snap Snapshot, damage repaintDamage) {
	if damage.full {
		dst.Clear()
	}
	f.paintRegions(dst, g, snap, damage.full || damage.regions)
	if damage.full && g.child != g.border {
		paintBorder(dst, g.border)
	}
	paintChild(dst, g.child, snap.Terminal, damage)
}

func paintBorder(dst uv.Screen, r uv.Rectangle) {
	put := func(x, y int, text string) { dst.SetCell(x, y, uv.NewCell(ansi.GraphemeWidth, text)) }
	for x := r.Min.X + 1; x < r.Max.X-1; x++ {
		put(x, r.Min.Y, borderHorizontal)
		put(x, r.Max.Y-1, borderHorizontal)
	}
	for y := r.Min.Y + 1; y < r.Max.Y-1; y++ {
		put(r.Min.X, y, borderVertical)
		put(r.Max.X-1, y, borderVertical)
	}
	put(r.Min.X, r.Min.Y, borderTopLeft)
	put(r.Max.X-1, r.Min.Y, borderTopRight)
	put(r.Min.X, r.Max.Y-1, borderBottomLeft)
	put(r.Max.X-1, r.Max.Y-1, borderBottomRight)
}

func paintChild(dst *uv.ScreenBuffer, area uv.Rectangle, term TerminalSnapshot, damage repaintDamage) {
	if term.Size.Cols <= 0 || term.Size.Rows <= 0 {
		return
	}
	// Cells form the backend's owned row-major viewport, including width-zero
	// continuation cells. Shortened rows clear their unavailable columns.
	rows := min(area.Dy(), term.Size.Rows)
	for y := range rows {
		if !damage.full && (y >= len(damage.rows) || !damage.rows[y]) {
			continue
		}
		start := y * term.Size.Cols
		end := max(0, min(len(term.Cells)-start, term.Size.Cols, area.Dx()))
		if end < area.Dx() {
			dst.ClearArea(uv.Rect(area.Min.X+end, area.Min.Y+y, area.Dx()-end, 1))
		}
		for x := 0; x < end; {
			cell := &term.Cells[start+x]
			if cell.Width > area.Dx()-x {
				dst.SetCell(area.Min.X+x, area.Min.Y+y, nil)
			}
			x += paintCell(dst, cell, area.Min.X+x, area.Min.Y+y, area.Max.X)
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
