package emulator

import (
	"fmt"
	"strings"
	"testing"

	ghostty "go.mitchellh.com/libghostty"
)

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
// Rows of one seed share no style, and neither do rows of different seeds.
func rgbRow(y, cols, seed int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\x1b[%d;1H", y)
	for x := range cols {
		fmt.Fprintf(&b, "\x1b[38;2;%d;%d;%dmx", seed%256, x, y)
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
	// Each write gives the last row six new RGB styles. Once a capture
	// leaves the table with more styles than the viewport has cells, besides
	// the default style, it drops the styles no cell references and
	// renumbers every row, clean ones included.
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
	if limit := 6*3 + 1; len(em.styles) > limit {
		t.Fatalf("compacted table holds %d styles, want at most %d", len(em.styles), limit)
	}
	assertStyles(t, "clone after compaction", resolvedStyles(t, clone), cloneStyles)

	if err := em.Resize(Size{Cols: 4, Rows: 2}); err != nil {
		t.Fatal(err)
	}
	resized := terminalState(t, em)
	assertStyles(t, "capture after resize", resolvedStyles(t, resized), renderStyles(t, em))
}

// rgbFrame writes every cell of a cols×rows viewport with its own RGB
// foreground. Frames of different seeds share no style.
func rgbFrame(cols, rows, seed int) string {
	var b strings.Builder
	for y := range rows {
		b.WriteString(rgbRow(y+1, cols, seed))
	}
	return b.String()
}

// lendState captures em into s, borrowing the terminal storage as the session
// does. The lent table may hold at most one style per cell plus the default
// style, so a snapshot of distinct styles is no larger than one whose cells
// each held their style, and every cell must resolve to libghostty's style.
func lendState(t *testing.T, em *Terminal, s *State, name string) {
	t.Helper()
	if err := em.UpdateState(s); err != nil {
		t.Fatal(err)
	}
	if limit := len(s.NativeCells) + 1; len(s.NativeStyles) > limit {
		t.Fatalf("%s: lent style table holds %d styles for %d cells, want at most %d", name, len(s.NativeStyles), len(s.NativeCells), limit)
	}
	assertStyles(t, name, resolvedStyles(t, *s), renderStyles(t, em))
}

func TestLentStyleTableHoldsAtMostOneStylePerCell(t *testing.T) {
	const cols, rows = 20, 6
	type step struct {
		output string
		// held is whether the output leaves a render hold whose preserved
		// frame waits for the read to convert it.
		held bool
		// firstDirty is the first row the capture converts; rows above it
		// keep the indices of earlier captures.
		firstDirty int
	}
	tests := []struct {
		name  string
		steps []step
	}{
		{
			name: "new styles in every cell of every capture",
			steps: []step{
				{output: rgbFrame(cols, rows, 0)},
				{output: rgbFrame(cols, rows, 1)},
				{output: rgbFrame(cols, rows, 2)},
				{output: rgbFrame(cols, rows, 3)},
				// No cell referenced the default style before the erase.
				{output: "\x1b[H\x1b[2J"},
			},
		},
		{
			// Each compaction drops the last row's previous styles and moves
			// its new ones down. Rewriting the row then finds the moved
			// styles, and the dropped ones are interned again.
			name: "new styles in the last row beside clean rows",
			steps: []step{
				{output: rgbFrame(cols, rows, 0)},
				{output: rgbRow(rows, cols, 1), firstDirty: rows - 1},
				{output: rgbRow(rows, cols, 2), firstDirty: rows - 1},
				{output: rgbRow(rows, cols, 2), firstDirty: rows - 1},
				{output: rgbRow(rows, cols, 1), firstDirty: rows - 1},
			},
		},
		{
			// The hold preserves frame 1 and leaves its conversion to the
			// read; frame 2, written in the hold, shows once it ends.
			name: "render hold begins before the conversion",
			steps: []step{
				{output: rgbFrame(cols, rows, 0)},
				{output: rgbFrame(cols, rows, 1) + "\x1b[?2026h" + rgbFrame(cols, rows, 2), held: true},
				{output: "\x1b[?2026l"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			em := newTerminal(t, cols, rows)
			var s State
			for i, st := range tt.steps {
				name := fmt.Sprintf("capture %d", i)
				writeTerminal(t, em, st.output)
				if em.holdPending != st.held {
					t.Fatalf("%s: conversion deferred to the read = %v, want %v", name, em.holdPending, st.held)
				}
				lendState(t, em, &s, name)
				if s.Held != st.held {
					t.Fatalf("%s: Held = %v, want %v", name, s.Held, st.held)
				}
				for y, dirty := range em.DirtyRows() {
					if dirty != (y >= st.firstDirty) {
						t.Fatalf("%s: dirty rows = %v, want rows %d to %d", name, em.DirtyRows(), st.firstDirty, rows-1)
					}
				}
				em.ClearDamage()
			}
		})
	}
}
