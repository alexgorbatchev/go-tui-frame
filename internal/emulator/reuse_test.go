package emulator

import (
	"runtime"
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

// cgoCalls reports the cgo calls f makes. The emulator starts no goroutines,
// so every call counted while f runs is f's.
func cgoCalls(f func()) int {
	before := runtime.NumCgoCall()
	f()
	return int(runtime.NumCgoCall() - before)
}

func TestInputStateReadsRoutingValuesOncePerMutation(t *testing.T) {
	em := newTerminal(t, 120, 40)
	var state State
	inputState := func() {
		if err := em.InputState(&state); err != nil {
			t.Fatal(err)
		}
	}
	inputState()
	// Besides one getter per routing mode, a read with mouse tracking off
	// calls three getters (active screen, Kitty flags, mouse tracking), the
	// key probe's three encoder calls, the mouse probe's four encoder
	// options, of which the size makes two calls, and three calls for the
	// probe's first event, which the encoder drops.
	const otherCalls = 3 + 3 + 5 + 3
	// Every write discards the input sample, even one that changes nothing.
	writeTerminal(t, em, unchangedMode)
	if calls := cgoCalls(inputState); calls > len(inputModes)+otherCalls {
		t.Errorf("InputState after a write made %d cgo calls, want at most %d for %d routing modes", calls, len(inputModes)+otherCalls, len(inputModes))
	}
	if calls := cgoCalls(inputState); calls != 0 {
		t.Errorf("InputState without a mutation made %d cgo calls, want 0", calls)
	}
	if allocs := testing.AllocsPerRun(20, inputState); allocs != 0 {
		t.Errorf("InputState without a mutation allocated %.0f times, want 0", allocs)
	}
}

func TestInputStateObservesEveryMutation(t *testing.T) {
	em := newTerminal(t, 8, 3)
	var input, full State
	tests := []struct {
		name  string
		write string
		// resize, when set, replaces the write.
		resize *Size
		want   func(State) bool
	}{
		{"cursor keys", "\x1b[?1h", nil, func(s State) bool { return s.Modes[ghostty.ModeDECCKM] }},
		{"SGR mouse format", "\x1b[?1006h", nil, func(s State) bool { return s.Modes[ghostty.ModeSGRMouse] }},
		{"bracketed paste", "\x1b[?2004h", nil, func(s State) bool { return s.Modes[ghostty.ModeBracketedPaste] }},
		{"button tracking", "\x1b[?1002h", nil, func(s State) bool {
			return s.MouseTracking && s.MouseTrackingMode == ghostty.MouseTrackingButton
		}},
		// Resetting another tracking mode turns tracking off while the
		// getter still reports the set 1002 bit.
		{"tracking reset", "\x1b[?1000l", nil, func(s State) bool {
			return s.MouseTracking && s.MouseTrackingMode == ghostty.MouseTrackingNone
		}},
		{"Kitty keyboard", "\x1b[>1u", nil, func(s State) bool { return s.KittyKeyboardFlags == ghostty.KittyKeyDisambiguate }},
		{"modifyOtherKeys 2", "\x1b[>4;2m", nil, func(s State) bool { return s.ModifyOtherKeys2 }},
		{"alternate screen", "\x1b[?1049h", nil, func(s State) bool { return s.Alternate }},
		{"resize", "", &Size{Cols: 10, Rows: 4}, func(s State) bool { return s.Size == Size{Cols: 10, Rows: 4} }},
		{"synchronized output", "\x1b[?2026h", nil, func(s State) bool { return s.Held }},
	}
	for _, tt := range tests {
		// Read before each mutation, so the next read follows a cached one.
		if err := em.InputState(&input); err != nil {
			t.Fatal(err)
		}
		if tt.resize != nil {
			if err := em.Resize(*tt.resize); err != nil {
				t.Fatal(err)
			}
		} else {
			writeTerminal(t, em, tt.write)
		}
		if err := em.InputState(&input); err != nil {
			t.Fatal(err)
		}
		if !tt.want(input) {
			t.Errorf("%s: InputState did not observe the mutation", tt.name)
		}
		if err := em.UpdateState(&full); err != nil {
			t.Fatal(err)
		}
		if !tt.want(full) {
			t.Errorf("%s: UpdateState after a cached InputState did not observe the mutation", tt.name)
		}
	}
	if err := em.ReleaseHold(); err != nil {
		t.Fatal(err)
	}
	if err := em.InputState(&input); err != nil || input.Held {
		t.Errorf("InputState after ReleaseHold = held %v, %v", input.Held, err)
	}
	// UpdateState reads the modes routing does not use at every capture.
	writeTerminal(t, em, "\x1b[4h")
	if err := em.UpdateState(&full); err != nil || !full.Modes[ghostty.ModeInsert] || !full.Modes[ghostty.ModeDECCKM] {
		t.Errorf("UpdateState after insert mode = insert %v, cursor keys %v, %v", full.Modes[ghostty.ModeInsert], full.Modes[ghostty.ModeDECCKM], err)
	}
}

func BenchmarkBorrowedState(b *testing.B) {
	const cols, rows = 120, 40
	// Erasing and refilling the display dirties every row, so the capture
	// converts every viewport cell.
	fullViewport := []byte("\x1b[H\x1b[2J" + strings.Repeat(strings.Repeat("x", cols), rows))
	for _, name := range []string{"input", "input unchanged", "unchanged", "one row", "full viewport"} {
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
				switch name {
				case "input":
					// Discard the sample as a write would, without the
					// write's own native calls.
					em.input.valid = false
					err = em.InputState(&state)
				case "input unchanged":
					err = em.InputState(&state)
				default:
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
			if cell.Selected != selected || state.NativeStyles[cell.StyleIndex] != *style {
				t.Fatalf("cell %d,%d differs from native selection/style", x, y)
			}
		}
	}
}
