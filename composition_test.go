package frame

import (
	"os/exec"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
)

func TestCompositionKeepsChildInsideStyledFrameAndClearsOldCells(t *testing.T) {
	f := New(exec.Command("sh"), "ready").Header(1, func(ctx DrawContext[string]) {
		ctx.View.SetCell(0, 0, &uv.Cell{Content: "H", Width: 1})
	}).Footer(1, func(ctx DrawContext[string]) {
		ctx.View.SetCell(0, 0, &uv.Cell{Content: "F", Width: 1})
	}).Border(true)
	g, err := f.layout(Size{Cols: 10, Rows: 8})
	if err != nil {
		t.Fatal(err)
	}
	buf := uv.NewScreenBuffer(10, 8)
	cells := make([]uv.Cell, g.child.Dx()*g.child.Dy())
	cells[0] = uv.Cell{Content: "C", Width: 1}
	cells[g.child.Dx()-1] = uv.Cell{Content: "界", Width: 2}
	snap := Snapshot{Terminal: TerminalSnapshot{Size: Size{Cols: g.child.Dx(), Rows: g.child.Dy()}, Cells: cells}}
	f.compose(buf, g, snap, true)
	for _, point := range []struct {
		x, y int
		text string
	}{
		{0, 0, "H"}, {0, 7, "F"}, {0, 1, "╭"}, {9, 6, "╯"}, {1, 2, "C"},
	} {
		if cell := buf.CellAt(point.x, point.y); cell == nil || cell.Content != point.text {
			t.Fatalf("cell (%d,%d) = %#v, want %q", point.x, point.y, cell, point.text)
		}
	}
	if cell := buf.CellAt(8, 2); cell != nil && cell.Content == "界" {
		t.Fatal("partial child grapheme reached border")
	}
	snap.Terminal.Cells = nil
	f.compose(buf, g, snap, false)
	if cell := buf.CellAt(1, 2); cell != nil && cell.Content == "C" {
		t.Fatal("cleared child retained an old cell")
	}
	if cell := buf.CellAt(0, 0); cell == nil || cell.Content != "H" {
		t.Fatal("cached header lost during child clear")
	}
}
