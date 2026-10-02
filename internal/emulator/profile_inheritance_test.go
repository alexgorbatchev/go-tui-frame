package emulator

import (
	"strings"
	"testing"

	ghostty "go.mitchellh.com/libghostty"
)

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
