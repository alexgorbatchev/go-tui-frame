package emulator

import (
	"image/color"
	"strings"
	"testing"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	ghostty "go.mitchellh.com/libghostty"
)

// paletteCells exercises every place a cell can carry a palette color:
// foreground, style background, underline color, an unreported entry and the
// background an erase stores in the cell content.
const paletteCells = "\x1b[31mA\x1b[0;38;5;200mB\x1b[0;41mC\x1b[0;4;58;5;1mD\x1b[0;32mE\x1b[0m\x1b[2;1H\x1b[41m\x1b[K\x1b[0m"

// assertPaletteCells checks paletteCells against the colors expected for
// entries 1 and 200. Entry 2 is never reported and stays explicit RGB.
func assertPaletteCells(t *testing.T, s State, one, high color.Color) {
	t.Helper()
	cells := s.Cells
	for _, got := range []struct {
		name        string
		value, want color.Color
	}{
		{"SGR 31 foreground", cells[0].Style.Fg, one},
		{"SGR 38;5;200 foreground", cells[1].Style.Fg, high},
		{"SGR 41 background", cells[2].Style.Bg, one},
		{"SGR 58;5;1 underline", cells[3].Style.UnderlineColor, one},
		{"unreported SGR 32 foreground", cells[4].Style.Fg, rgbColor(s.Colors.Palette[2])},
	} {
		if got.value != got.want {
			t.Errorf("%s = %#v (%T); want %#v (%T)", got.name, got.value, got.value, got.want, got.want)
		}
	}
	for x, cell := range cells[s.Size.Cols : 2*s.Size.Cols] {
		if cell.Style.Bg != one {
			t.Fatalf("erased cell %d background = %#v (%T); want %#v (%T)", x, cell.Style.Bg, cell.Style.Bg, one, one)
		}
	}
}

var reportedPalette = &Profile{Palette: map[uint8]ghostty.ColorRGB{1: {R: 0xdc, G: 0x32, B: 0x2f}, 200: {R: 0x65, G: 0x43, B: 0x21}}}

func TestReportedPaletteEntriesStayIndexedWhereTheOuterProfileKeepsThem(t *testing.T) {
	for _, tt := range []struct {
		outer colorprofile.Profile
		// high is entry 200 while it keeps the reported value.
		high color.Color
	}{
		{colorprofile.ANSI256, ansi.IndexedColor(200)},
		// A 16-color renderer maps indexes above 15 through a fixed table
		// that ignores the host's color, so they keep the reported RGB.
		{colorprofile.ANSI, color.RGBA{R: 0x65, G: 0x43, B: 0x21, A: 255}},
	} {
		t.Run(tt.outer.String(), func(t *testing.T) {
			em, err := New(Options{Size: Size{Cols: 8, Rows: 2}, Profile: reportedPalette, OuterColorProfile: tt.outer})
			if err != nil {
				t.Fatal(err)
			}
			defer em.Close()
			writeTerminal(t, em, paletteCells)
			for _, step := range []struct {
				name, sequence string
				one, high      color.Color
			}{
				{"reported", "", ansi.BasicColor(1), tt.high},
				{"child override", "\x1b]4;1;#123456;200;#abcdef\a", color.RGBA{R: 0x12, G: 0x34, B: 0x56, A: 255}, color.RGBA{R: 0xab, G: 0xcd, B: 0xef, A: 255}},
				{"child reset", "\x1b]104\a", ansi.BasicColor(1), tt.high},
			} {
				writeTerminal(t, em, step.sequence)
				t.Run(step.name, func(t *testing.T) { assertPaletteCells(t, terminalState(t, em), step.one, step.high) })
			}
		})
	}
}

func TestPaletteStaysExplicitRGB(t *testing.T) {
	for _, tt := range []struct {
		name    string
		profile *Profile
		outer   colorprofile.Profile
	}{
		// InheritTerminal(false) supplies no profile, so no entry is host-owned.
		{"not inherited", nil, colorprofile.ANSI256},
		{"true color outer terminal", reportedPalette, colorprofile.TrueColor},
		// NO_COLOR and TERM=dumb renderers draw no colors at all.
		{"colorless outer terminal", reportedPalette, colorprofile.ASCII},
		{"outer output is not a terminal", reportedPalette, colorprofile.NoTTY},
		{"unknown outer profile", reportedPalette, colorprofile.Unknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			em, err := New(Options{Size: Size{Cols: 8, Rows: 2}, Profile: tt.profile, OuterColorProfile: tt.outer})
			if err != nil {
				t.Fatal(err)
			}
			defer em.Close()
			writeTerminal(t, em, paletteCells)
			s := terminalState(t, em)
			assertPaletteCells(t, s, rgbColor(s.Colors.Palette[1]), rgbColor(s.Colors.Palette[200]))
		})
	}
}

func TestInheritedDefaultsStayNativeAndSurviveReset(t *testing.T) {
	fg, bg := ghostty.ColorRGB{R: 0xd0, G: 0xc0, B: 0xb0}, ghostty.ColorRGB{R: 0x20, G: 0x30, B: 0x40}
	style, blink := ghostty.TerminalCursorStyleBar, true
	scheme := ghostty.ColorSchemeLight
	profile := &Profile{Foreground: &fg, Background: &bg, CursorStyle: &style, CursorBlink: &blink, Scheme: &scheme,
		Palette: map[uint8]ghostty.ColorRGB{1: {R: 0x12, G: 0x34, B: 0x56}}, Modes: map[ghostty.Mode]bool{ghostty.ModeDECCKM: true}}
	em, err := New(Options{Size: Size{Cols: 10, Rows: 3}, Profile: profile})
	if err != nil {
		t.Fatal(err)
	}
	defer em.Close()
	s := terminalState(t, em)
	if s.Cells[0].Style.Fg != nil || s.Cells[0].Style.Bg != nil {
		t.Error("unchanged host defaults must use default rendition so terminal opacity and default-color policies apply")
	}
	writeTerminal(t, em, "\x1b]10;#111111\x07\x1b]11;#222222\x07\x1b]4;1;#333333\x07\x1b[?1l\x1b[2 q")
	s = terminalState(t, em)
	if s.Cells[0].Style.Fg == nil || s.Cells[0].Style.Bg == nil {
		t.Error("child OSC overrides must render explicit colors")
	}
	writeTerminal(t, em, "\x1bc")
	// RIS keeps dynamic OSC colors. Their dedicated reset controls restore
	// the configured host defaults instead of a built-in palette.
	writeTerminal(t, em, "\x1b]110\x07\x1b]111\x07\x1b]104;1\x07")
	s = terminalState(t, em)
	if s.Colors.Foreground != fg || s.Colors.Background != bg || s.Colors.Palette[1] != profile.Palette[1] || !s.Modes[ghostty.ModeDECCKM] || s.Cursor.VisualStyle != ghostty.CursorVisualStyleBar || !s.Cursor.Blinking {
		t.Errorf("reset lost inherited defaults: cursor=%+v fg=%+v bg=%+v palette1=%+v", s.Cursor, s.Colors.Foreground, s.Colors.Background, s.Colors.Palette[1])
	}
	writeTerminal(t, em, "\x1b[?996n")
	var replies strings.Builder
	for _, effect := range em.Effects() {
		if effect.Kind == Reply {
			replies.Write(effect.Bytes)
		}
	}
	if !strings.Contains(replies.String(), "\x1b[?997;2n") {
		t.Fatalf("scheme query reply=%q", replies.String())
	}
}
