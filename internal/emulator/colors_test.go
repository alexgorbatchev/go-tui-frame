package emulator

import (
	"fmt"
	"image/color"
	"testing"

	ghostty "go.mitchellh.com/libghostty"
)

func TestOSCDefaultColorsRepaintExistingCells(t *testing.T) {
	for _, tt := range []struct {
		name, change, reset string
		foreground          bool
	}{
		{"foreground", "\x1b]10;#123456\a", "\x1b]110\x1b\\", true},
		{"background", "\x1b]11;#123456\x1b\\", "\x1b]111\a", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			em := newTerminal(t, 8, 2)
			writeTerminal(t, em, "one\r\ntwo\x1b[1;5H\x1b[38;2;1;2;3;48;2;4;5;6mX\x1b[0m")
			old := terminalState(t, em)
			em.ClearDamage()
			fg, bg := old.Colors.Foreground, old.Colors.Background
			changed := ghostty.ColorRGB{R: 0x12, G: 0x34, B: 0x56}
			if tt.foreground {
				fg = changed
			} else {
				bg = changed
			}
			writeTerminal(t, em, tt.change)
			next := terminalState(t, em)
			assertDefaultColors(t, next, fg, bg, 4)
			for y, dirty := range em.DirtyRows() {
				if !dirty {
					t.Fatalf("color-only change left row %d clean", y)
				}
			}
			if next.Cells[4] != old.Cells[4] {
				t.Fatal("default color override changed an explicit SGR color")
			}
			assertDefaultColors(t, old, old.Colors.Foreground, old.Colors.Background, 4)
			writeTerminal(t, em, tt.reset)
			reset := terminalState(t, em)
			assertDefaultColors(t, reset, old.Colors.Foreground, old.Colors.Background, 4)
			effectiveFG, fgErr := em.native.ColorForeground()
			effectiveBG, bgErr := em.native.ColorBackground()
			if fgErr != nil || bgErr != nil || effectiveFG == nil || effectiveBG == nil || *effectiveFG != reset.Colors.Foreground || *effectiveBG != reset.Colors.Background {
				t.Fatalf("reset native/render colors disagree: foreground=%v (%v), background=%v (%v), render=%#v", effectiveFG, fgErr, effectiveBG, bgErr, reset.Colors)
			}
		})
	}
}

func TestOSCDefaultColorsRespectRenderHoldAndReverseMode(t *testing.T) {
	em := newTerminal(t, 8, 2)
	writeTerminal(t, em, "one\r\ntwo")
	old := terminalState(t, em)
	em.ClearDamage()
	writeTerminal(t, em, "\x1b[?2026h\x1b]10;#123456\a")
	held := terminalState(t, em)
	assertDefaultColors(t, held, old.Colors.Foreground, old.Colors.Background, -1)
	if !held.Held {
		t.Fatal("color change ended the render hold")
	}
	writeTerminal(t, em, "\x1b[?2026l")
	fg := ghostty.ColorRGB{R: 0x12, G: 0x34, B: 0x56}
	assertDefaultColors(t, terminalState(t, em), fg, old.Colors.Background, -1)
	writeTerminal(t, em, "\x1b[?5h")
	assertDefaultColors(t, terminalState(t, em), old.Colors.Background, fg, -1)
	writeTerminal(t, em, "\x1b[?5l\x1b]110\a")
	assertDefaultColors(t, terminalState(t, em), old.Colors.Foreground, old.Colors.Background, -1)
}

func TestOSCDefaultColorQueriesAgreeWithRenderedColors(t *testing.T) {
	em := newTerminal(t, 8, 2)
	for _, sequence := range []string{"text", "\x1b]10;#123456\a\x1b]11;#abcdef\a", "\x1b]110\a\x1b]111\a"} {
		writeTerminal(t, em, sequence)
		s := terminalState(t, em)
		writeTerminal(t, em, "\x1b]10;?\x1b\\\x1b]11;?\x1b\\")
		replies := em.Effects()
		if len(replies) != 2 {
			t.Fatalf("color queries after %q produced %d replies", sequence, len(replies))
		}
		for i, rgb := range []ghostty.ColorRGB{s.Colors.Foreground, s.Colors.Background} {
			want := fmt.Sprintf("\x1b]%d;rgb:%02x%02x/%02x%02x/%02x%02x\x1b\\", 10+i, rgb.R, rgb.R, rgb.G, rgb.G, rgb.B, rgb.B)
			if replies[i].Kind != Reply || string(replies[i].Bytes) != want {
				t.Fatalf("color query after %q: %#v; want %q", sequence, replies[i], want)
			}
		}
	}
}

func assertDefaultColors(t *testing.T, s State, fg, bg ghostty.ColorRGB, explicit int) {
	t.Helper()
	if s.Colors.Foreground != fg || s.Colors.Background != bg {
		t.Fatalf("render colors: foreground=%#v, background=%#v; want %#v, %#v", s.Colors.Foreground, s.Colors.Background, fg, bg)
	}
	wantFG := color.RGBA{R: fg.R, G: fg.G, B: fg.B, A: 255}
	wantBG := color.RGBA{R: bg.R, G: bg.G, B: bg.B, A: 255}
	for i, cell := range s.Cells {
		if i != explicit && (cell.Style.Fg != wantFG || cell.Style.Bg != wantBG) {
			t.Fatalf("cell %d has stale default colors: %#v", i, cell.Style)
		}
	}
}
