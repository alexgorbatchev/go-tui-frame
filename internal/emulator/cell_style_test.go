package emulator

import (
	"fmt"
	"strings"
	"testing"
	"unsafe"

	ghostty "go.mitchellh.com/libghostty"
)

func TestNativeCellHoldsStyleIndex(t *testing.T) {
	// The packed cell and the width are 8 bytes each; a style index and the
	// selection flag share the last word. A full ghostty.Style would add 72.
	if got := unsafe.Sizeof(NativeCell{}); got > 24 {
		t.Fatalf("NativeCell is %d bytes, want at most 24", got)
	}
}

// renderStyles reads every viewport cell's full style with libghostty's own
// getter, from the render state the last capture updated, in cell order.
func renderStyles(t *testing.T, em *Terminal) []ghostty.Style {
	t.Helper()
	if err := em.render.RowIterator(em.rows); err != nil {
		t.Fatal(err)
	}
	styles := make([]ghostty.Style, 0, em.size.Cols*em.size.Rows)
	for em.rows.Next() {
		if err := em.rows.Cells(em.cells); err != nil {
			t.Fatal(err)
		}
		for x := range em.size.Cols {
			if err := em.cells.Select(uint16(x)); err != nil {
				t.Fatal(err)
			}
			style, err := em.cells.Style()
			if err != nil {
				t.Fatal(err)
			}
			styles = append(styles, *style)
		}
	}
	return styles
}

// resolvedStyles resolves every native cell's style through the state's own
// style table.
func resolvedStyles(t *testing.T, s State) []ghostty.Style {
	t.Helper()
	styles := make([]ghostty.Style, len(s.NativeCells))
	for i, cell := range s.NativeCells {
		if int(cell.StyleIndex) >= len(s.NativeStyles) {
			t.Fatalf("cell %d has style index %d outside its %d-style table", i, cell.StyleIndex, len(s.NativeStyles))
		}
		styles[i] = s.NativeStyles[cell.StyleIndex]
	}
	return styles
}

func assertStyles(t *testing.T, name string, got, want []ghostty.Style) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d cell styles, want %d", name, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: cell %d resolves to %+v, want %+v", name, i, got[i], want[i])
		}
	}
}

// rgbRow writes cols cells to row y, each with its own RGB foreground.
func rgbRow(y, cols, seed int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\x1b[%d;1H", y)
	for x := range cols {
		fmt.Fprintf(&b, "\x1b[38;2;%d;%d;7mx", seed%256, x)
	}
	b.WriteString("\x1b[0m")
	return b.String()
}

func TestNativeStylesResolveAcrossCapturesAndClones(t *testing.T) {
	em := newTerminal(t, 6, 3)
	// The cursor ends on the last row, so later writes there leave the first
	// two rows clean.
	writeTerminal(t, em, "\x1b[31maa\x1b[1;53mbb\x1b[0m\r\n\x1b[4:3;38;2;1;2;3mcc\x1b[0m\x1b[3;1H")
	first := terminalState(t, em)
	firstStyles := renderStyles(t, em)
	assertStyles(t, "first capture", resolvedStyles(t, first), firstStyles)
	if overline := first.NativeStyles[first.NativeCells[2].StyleIndex]; !overline.Overline() {
		t.Fatal("the table lost the native overline")
	}
	em.ClearDamage()

	// Only the last row changes, so the first two keep the indices of the
	// first capture while the capture interns the new row's styles.
	writeTerminal(t, em, "\x1b[3;1H\x1b[32;9mdd\x1b[0m")
	second := terminalState(t, em)
	if dirty := em.DirtyRows(); dirty[0] || dirty[1] || !dirty[2] {
		t.Fatalf("dirty rows = %v, want only the last row recaptured", dirty)
	}
	assertStyles(t, "capture with clean rows", resolvedStyles(t, second), renderStyles(t, em))
	assertStyles(t, "first snapshot after a later capture", resolvedStyles(t, first), firstStyles)

	clone := CloneState(second)
	cloneStyles := resolvedStyles(t, clone)
	// Each write gives the last row six new RGB styles. The table compacts
	// once it holds more than twice the viewport's cells, and the full
	// recapture that follows assigns every row, clean ones included, an
	// index into the rebuilt table.
	compacted := false
	for seed := 0; !compacted; seed++ {
		if seed == 32 {
			t.Fatalf("the style table grew to %d entries without compacting", len(em.styles))
		}
		before := len(em.styles)
		writeTerminal(t, em, rgbRow(3, 6, seed))
		state := terminalState(t, em)
		compacted = len(em.styles) < before
		assertStyles(t, fmt.Sprintf("capture %d", seed), resolvedStyles(t, state), renderStyles(t, em))
	}
	if limit := 2 * 6 * 3; len(em.styles) > limit {
		t.Fatalf("compacted table holds %d styles, want at most %d", len(em.styles), limit)
	}
	assertStyles(t, "clone after compaction", resolvedStyles(t, clone), cloneStyles)

	if err := em.Resize(Size{Cols: 4, Rows: 2}); err != nil {
		t.Fatal(err)
	}
	resized := terminalState(t, em)
	assertStyles(t, "capture after resize", resolvedStyles(t, resized), renderStyles(t, em))
}
