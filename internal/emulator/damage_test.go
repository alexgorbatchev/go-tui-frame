package emulator

import (
	"fmt"
	"image/color"
	"strings"
	"testing"

	ghostty "go.mitchellh.com/libghostty"
)

func TestCaptureConsumesNativeDamageAndPreservesOwnedFrames(t *testing.T) {
	em := newTerminal(t, 8, 3)
	writeTerminal(t, em, "one\r\ntwo\r\nthree")
	old := terminalState(t, em)
	if dirty, err := em.render.Dirty(); err != nil || dirty != ghostty.RenderStateDirtyFalse {
		t.Fatalf("captured native damage was not consumed: %v, %v", dirty, err)
	}
	writeTerminal(t, em, "\x1b[2;1HTWO")
	next := terminalState(t, em)
	if next.Cells[0].Content != "o" || next.Cells[8].Content != "T" || next.Cells[16].Content != "t" || old.Cells[8].Content != "t" {
		t.Fatal("incremental capture changed a clean row or an owned snapshot")
	}
	writeTerminal(t, em, "\x1b[3;1H\nlast")
	scrolled := terminalState(t, em)
	if scrolled.Cells[0].Content != "T" || scrolled.Cells[8].Content != "t" || scrolled.Cells[16].Content != "l" {
		t.Fatal("incremental capture kept stale rows after scrolling")
	}
	if err := em.native.SetColorForeground(&ghostty.ColorRGB{R: 0x12, G: 0x34, B: 0x56}); err != nil {
		t.Fatal(err)
	}
	recolored := terminalState(t, em)
	if recolored.Cells[0].Style.Fg != (color.RGBA{R: 0x12, G: 0x34, B: 0x56, A: 255}) {
		t.Fatalf("default-color change left a cached style: %#v", recolored.Cells[0].Style)
	}
	if err := em.Resize(Size{Cols: 12, Rows: 4}); err != nil {
		t.Fatal(err)
	}
	resized := terminalState(t, em)
	if len(resized.Cells) != 48 || len(resized.NativeCells) != 48 || resized.Cells[0].Width == 0 {
		t.Fatal("resize did not initialize the new viewport")
	}
}

func BenchmarkStateCapture(b *testing.B) {
	for _, tt := range []struct {
		name string
		text string
	}{{"unchanged", ""}, {"one row", "\x1b[Hupdated"}} {
		b.Run(tt.name, func(b *testing.B) {
			em, err := New(Options{Size: Size{Cols: 120, Rows: 40}})
			if err != nil {
				b.Fatal(err)
			}
			defer em.Close()
			if _, err := em.State(); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := em.Write([]byte(tt.text)); err != nil {
					b.Fatal(err)
				}
				if _, err := em.State(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkStyledCapture measures an internal capture of a viewport whose
// every row is damaged and styled. Few styles repeats three runs on each row;
// style per cell gives every cell its own direct RGB color, as gradients and
// image-to-ANSI output do. The styles stay the same across iterations and
// only the text alternates, so the write that damages the rows is excluded.
func BenchmarkStyledCapture(b *testing.B) {
	for _, tt := range []struct {
		name string
		size Size
		row  func(cols, y int, text string) string
	}{
		{"few styles/120x40", Size{Cols: 120, Rows: 40}, func(cols, _ int, text string) string {
			run := strings.Repeat(text, cols/3)
			return "\x1b[1;38;5;33m" + run + "\x1b[0m" + run + "\x1b[4;38;2;11;22;33m" + run + "\x1b[0m"
		}},
		{"style per cell/40x24", Size{Cols: 40, Rows: 24}, styledCellRow},
		{"style per cell/200x24", Size{Cols: 200, Rows: 24}, styledCellRow},
		{"style per cell/400x2", Size{Cols: 400, Rows: 2}, styledCellRow},
		{"style per cell/1000x1", Size{Cols: 1000, Rows: 1}, styledCellRow},
	} {
		b.Run(tt.name, func(b *testing.B) {
			em, err := New(Options{Size: tt.size})
			if err != nil {
				b.Fatal(err)
			}
			defer em.Close()
			var frames [2][]byte
			for i, text := range []string{"a", "b"} {
				var frame strings.Builder
				for y := range tt.size.Rows {
					fmt.Fprintf(&frame, "\x1b[%d;1H%s", y+1, tt.row(tt.size.Cols, y, text))
				}
				frames[i] = []byte(frame.String())
			}
			var state State
			capture := func(i int) {
				if _, err := em.Write(frames[i%2]); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				if err := em.UpdateState(&state); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				if !em.DirtyRows()[tt.size.Rows-1] {
					b.Fatal("frame left the last row clean")
				}
				em.ClearDamage()
			}
			b.StopTimer()
			capture(0)
			capture(1)
			b.ReportAllocs()
			b.ResetTimer()
			b.StopTimer()
			for i := range b.N {
				capture(i)
			}
		})
	}
}

func styledCellRow(cols, y int, text string) string {
	var row strings.Builder
	for x := range cols {
		n := y*cols + x
		fmt.Fprintf(&row, "\x1b[38;2;%d;%d;7m%s", uint8(n>>8), uint8(n), text)
	}
	return row.String() + "\x1b[0m"
}
