package frame

import (
	"os/exec"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

func TestDrawContextAcceptsScreenBuffer(t *testing.T) {
	view := uv.NewScreenBuffer(8, 2)
	view.Method = ansi.GraphemeWidth
	ctx := DrawContext[string]{View: view, Data: "workspace\nready"}
	uv.NewStyledString(ctx.Data).Draw(ctx.View, ctx.View.Bounds())
	if got := view.Render(); got != "workspac\nready   " {
		t.Fatalf("native screen drawing = %q", got)
	}
}

func TestRegionScreenPreservesGraphemesAndPushUpdates(t *testing.T) {
	f := New(exec.Command("sh"), "👩‍💻").Header(1, func(ctx DrawContext[string]) {
		uv.NewStyledString(ctx.Data).Draw(ctx.View, ctx.View.Bounds())
	})
	g, err := f.layout(Size{Cols: 8, Rows: 4})
	if err != nil {
		t.Fatal(err)
	}
	buf := uv.NewScreenBuffer(8, 4)
	f.paintRegions(buf, g, Snapshot{}, true)
	if cell := buf.CellAt(0, 0); cell.Content != "👩‍💻" || cell.Width != 2 {
		t.Fatalf("grapheme cell = %#v", cell)
	}
	if err := f.InvalidateHeader("ready"); err != nil {
		t.Fatal(err)
	}
	f.paintRegions(buf, g, Snapshot{}, false)
	if cell := buf.CellAt(0, 0); cell.Content != "r" || cell.Width != 1 {
		t.Fatalf("updated screen cell = %#v", cell)
	}
	if cell := buf.CellAt(5, 0); cell.Content != " " || cell.Width != 1 {
		t.Fatalf("stale cell after update = %#v", cell)
	}
}
