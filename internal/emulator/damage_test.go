package emulator

import (
	"image/color"
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
	if err := em.native.SetColorBackground(&ghostty.ColorRGB{}); err != nil {
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
