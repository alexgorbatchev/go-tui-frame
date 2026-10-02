package frame

import (
	"testing"

	"github.com/alexgorbatchev/go-tui-frame/internal/emulator"
	"github.com/alexgorbatchev/go-tui-frame/internal/input"
	"github.com/charmbracelet/x/ansi"
	ghostty "go.mitchellh.com/libghostty"
)

func preferenceReply(t *testing.T, c *console, reply string) bool {
	t.Helper()
	packets, err := input.New().Feed([]byte(reply))
	if err != nil || len(packets) != 1 {
		t.Fatalf("decode reply %q: packets=%d error=%v", reply, len(packets), err)
	}
	return c.consumeReply(packets[0])
}

func TestPreferenceRepliesValidateAndRetainOnlySolicitedValues(t *testing.T) {
	c := &console{inherit: true, pending: make(map[ansi.DECMode]bool)}
	c.preferenceQueries()
	for _, reply := range []string{
		"\x1b]11;rgb:zz/00/00\x07",
		"\x1b]4;256;#123456\x07",
		"\x1b]4;1\x07",
		"\x1b]13;#123456\x07",
		"\x1bP1$r9 q\x1b\\",
	} {
		if preferenceReply(t, c, reply) {
			t.Errorf("invalid or unsolicited reply consumed: %q", reply)
		}
	}
	for _, reply := range []string{
		"\x1b]10;rgb:aaaa/bbbb/cccc\x1b\\",
		"\x1b]11;#203040\x07",
		"\x1b]4;1;#123456;200;rgb:65/43/21\x07",
		"\x1bP1$r4 q\x1b\\",
		"\x1b[?997;2n",
		"\x1b[?1;1$y",
		"\x1b[4;2$y",
	} {
		if !preferenceReply(t, c, reply) {
			t.Errorf("solicited reply was not consumed: %q", reply)
		}
	}
	if preferenceReply(t, c, "\x1b]11;#000000\x07") {
		t.Error("duplicate reply was consumed")
	}
	em, err := emulator.New(c.childOptions(emulator.Size{Cols: 10, Rows: 3}))
	if err != nil {
		t.Fatal(err)
	}
	defer em.Close()
	s, err := em.State()
	if err != nil {
		t.Fatal(err)
	}
	if s.Colors.Foreground != (ghostty.ColorRGB{R: 0xaa, G: 0xbb, B: 0xcc}) || s.Colors.Background != (ghostty.ColorRGB{R: 0x20, G: 0x30, B: 0x40}) || s.Colors.Palette[200] != (ghostty.ColorRGB{R: 0x65, G: 0x43, B: 0x21}) || !s.Modes[ghostty.ModeDECCKM] || s.Modes[ghostty.ModeInsert] || s.Cursor.VisualStyle != ghostty.CursorVisualStyleUnderline || s.Cursor.Blinking {
		t.Errorf("decoded preferences were not applied: fg=%+v bg=%+v palette200=%+v cursor=%+v", s.Colors.Foreground, s.Colors.Background, s.Colors.Palette[200], s.Cursor)
	}
}

func TestLatePreferenceRepliesDoNotChangeChildDefaults(t *testing.T) {
	c := &console{inherit: true, pending: make(map[ansi.DECMode]bool)}
	c.preferenceQueries()
	c.preferences.closed = true
	for _, reply := range []string{"\x1b]11;#123456\x07", "\x1b]4;1;#123456\x07", "\x1bP1$r6 q\x1b\\", "\x1b[?997;1n", "\x1b[?1;1$y"} {
		if !preferenceReply(t, c, reply) {
			t.Errorf("late solicited reply was forwarded as child input: %q", reply)
		}
	}
	em, err := emulator.New(c.childOptions(emulator.Size{Cols: 10, Rows: 3}))
	if err != nil {
		t.Fatal(err)
	}
	defer em.Close()
	s, err := em.State()
	if err != nil {
		t.Fatal(err)
	}
	if s.Colors.Background != (ghostty.ColorRGB{}) || s.Modes[ghostty.ModeDECCKM] || s.Cursor.VisualStyle != ghostty.CursorVisualStyleBlock {
		t.Errorf("late replies changed defaults: bg=%+v cursor=%+v", s.Colors.Background, s.Cursor)
	}
}

func TestUnsupportedCursorStyleReplyKeepsDefaults(t *testing.T) {
	c := &console{inherit: true, pending: make(map[ansi.DECMode]bool)}
	c.preferenceQueries()
	if !preferenceReply(t, c, "\x1bP0$r\x1b\\") {
		t.Fatal("unsupported DECRQSS reply would reach the child as input")
	}
	if c.preferences.cursorPending || c.preferences.cursorStyle != nil {
		t.Fatal("unsupported reply must finish the probe without inventing a cursor style")
	}
}

func TestReportedKeyboardPreferencesSeedChildEncoder(t *testing.T) {
	c := &console{inherit: true, kittyPending: true, modifyPending: true}
	c.preferenceQueries()
	if !preferenceReply(t, c, "\x1b[?3u") || !preferenceReply(t, c, "\x1b[>4;2m") {
		t.Fatal("keyboard preference replies were not consumed")
	}
	em, err := emulator.New(c.childOptions(emulator.Size{Cols: 10, Rows: 3}))
	if err != nil {
		t.Fatal(err)
	}
	defer em.Close()
	s, err := em.State()
	if err != nil {
		t.Fatal(err)
	}
	if s.KittyKeyboardFlags != ghostty.KittyKeyDisambiguate|ghostty.KittyKeyReportEvents || !s.ModifyOtherKeys2 {
		t.Errorf("child keyboard protocol flags=%d modifyOtherKeys2=%v", s.KittyKeyboardFlags, s.ModifyOtherKeys2)
	}
}
