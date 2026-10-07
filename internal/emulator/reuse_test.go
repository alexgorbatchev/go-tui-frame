package emulator

import (
	"slices"
	"strings"
	"testing"

	ghostty "go.mitchellh.com/libghostty"
)

func TestBorrowedStateReusesStorageAndAccumulatesDamage(t *testing.T) {
	em := newTerminal(t, 8, 3)
	var state State
	if err := em.UpdateState(&state); err != nil {
		t.Fatal(err)
	}
	cells, native := &state.Cells[0], &state.NativeCells[0]
	em.ClearDamage()
	writeTerminal(t, em, "one")
	if err := em.UpdateState(&state); err != nil {
		t.Fatal(err)
	}
	owned := CloneState(state)
	writeTerminal(t, em, "\x1b[2;1Htwo")
	if err := em.UpdateState(&state); err != nil {
		t.Fatal(err)
	}
	if &state.Cells[0] != cells || &state.NativeCells[0] != native {
		t.Fatal("same-sized internal capture replaced cell storage")
	}
	if rows := em.DirtyRows(); len(rows) != 3 || !rows[0] || !rows[1] || rows[2] {
		t.Fatalf("unpainted row damage did not accumulate: %v", rows)
	}
	if owned.Cells[8].Content != " " || state.Cells[8].Content != "t" {
		t.Fatal("borrowed state invalidated an owned snapshot")
	}
	writeTerminal(t, em, "\x1b[3;1Hthree")
	if err := em.UpdateState(&state); err != nil {
		t.Fatal(err)
	}
	for _, dirty := range em.DirtyRows() {
		if !dirty {
			t.Fatal("a later capture discarded earlier unpainted row damage")
		}
	}
	em.ClearDamage()
	writeTerminal(t, em, "\x1b[?1h")
	if err := em.InputState(&state); err != nil || !state.Modes[ghostty.ModeDECCKM] {
		t.Fatalf("input mode capture = %v, %v", state.Modes, err)
	}
	for _, dirty := range em.DirtyRows() {
		if dirty {
			t.Fatal("input mode observation dirtied the borrowed display")
		}
	}
}

func BenchmarkBorrowedState(b *testing.B) {
	const cols, rows = 120, 40
	// Erasing and refilling the display dirties every row, so the capture
	// converts every viewport cell.
	fullViewport := []byte("\x1b[H\x1b[2J" + strings.Repeat(strings.Repeat("x", cols), rows))
	for _, name := range []string{"input", "unchanged", "one row", "full viewport"} {
		b.Run(name, func(b *testing.B) {
			em, err := New(Options{Size: Size{Cols: cols, Rows: rows}})
			if err != nil {
				b.Fatal(err)
			}
			defer em.Close()
			var state State
			if err := em.UpdateState(&state); err != nil {
				b.Fatal(err)
			}
			text := []byte("\x1b[Hupdated")
			if name == "full viewport" {
				text = fullViewport
				em.ClearDamage()
				if _, err := em.Write(text); err != nil {
					b.Fatal(err)
				}
				if err := em.UpdateState(&state); err != nil {
					b.Fatal(err)
				}
				if slices.Contains(em.DirtyRows(), false) {
					b.Fatalf("full-viewport output left rows clean: %v", em.DirtyRows())
				}
				em.ClearDamage()
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if name == "one row" || name == "full viewport" {
					if _, err := em.Write(text); err != nil {
						b.Fatal(err)
					}
				}
				if name == "input" {
					err = em.InputState(&state)
				} else {
					err = em.UpdateState(&state)
				}
				if err != nil {
					b.Fatal(err)
				}
				em.ClearDamage()
			}
		})
	}
}

func TestPlainRowCaptureReusesStyles(t *testing.T) {
	em := newTerminal(t, 120, 4)
	var state State
	if err := em.UpdateState(&state); err != nil {
		t.Fatal(err)
	}
	unchanged := testing.AllocsPerRun(20, func() {
		if err := em.UpdateState(&state); err != nil {
			t.Fatal(err)
		}
	})
	text := []byte("\x1b[Hupdated")
	row := testing.AllocsPerRun(20, func() {
		if _, err := em.Write(text); err != nil {
			t.Fatal(err)
		}
		if err := em.UpdateState(&state); err != nil {
			t.Fatal(err)
		}
	})
	if row-unchanged >= float64(4*state.Size.Cols) {
		t.Fatalf("plain row allocated per-cell style snapshots: %.0f additional allocations", row-unchanged)
	}
}

func TestCapturePreservesRowSelectionAndFullStyles(t *testing.T) {
	em := newTerminal(t, 8, 3)
	writeTerminal(t, em, "plain\r\n\x1b[1;53;4:3mstyled\x1b[0m")
	start, err := em.native.GridRef(ghostty.Point{Tag: ghostty.PointTagActive, X: 1, Y: 0})
	if err != nil {
		t.Fatal(err)
	}
	end, err := em.native.GridRef(ghostty.Point{Tag: ghostty.PointTagActive, X: 3, Y: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := em.native.SetSelection(&ghostty.Selection{Start: *start, End: *end}); err != nil {
		t.Fatal(err)
	}
	state := terminalState(t, em)
	if err := em.render.RowIterator(em.rows); err != nil {
		t.Fatal(err)
	}
	for y := 0; em.rows.Next(); y++ {
		if err := em.rows.Cells(em.cells); err != nil {
			t.Fatal(err)
		}
		for x := range state.Size.Cols {
			if err := em.cells.Select(uint16(x)); err != nil {
				t.Fatal(err)
			}
			selected, err := em.cells.Selected()
			if err != nil {
				t.Fatal(err)
			}
			style, err := em.cells.Style()
			if err != nil {
				t.Fatal(err)
			}
			cell := state.NativeCells[y*state.Size.Cols+x]
			if cell.Selected != selected || cell.Style != *style {
				t.Fatalf("cell %d,%d differs from native selection/style", x, y)
			}
		}
	}
}
