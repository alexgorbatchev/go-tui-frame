package frame

import (
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/alexgorbatchev/go-tui-frame/internal/emulator"
	"github.com/alexgorbatchev/go-tui-frame/internal/input"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	ghostty "go.mitchellh.com/libghostty"
)

func decodePacket(t *testing.T, raw string) input.Packet {
	t.Helper()
	packets, err := input.New().Feed([]byte(raw))
	if err != nil || len(packets) != 1 {
		t.Fatalf("decode packet %q: packets=%d error=%v", raw, len(packets), err)
	}
	return packets[0]
}

func preferenceReply(t *testing.T, c *console, reply string) bool {
	t.Helper()
	return c.consumeReply(decodePacket(t, reply))
}

func TestPreferenceParsingDoesNotAllocatePerPacket(t *testing.T) {
	for _, tt := range []struct {
		name    string
		inherit bool
		// settle is a reply consumed before measuring, so raw repeats a
		// query that is no longer pending.
		settle, raw string
		event       any
	}{
		{name: "key", raw: "a", event: uv.KeyPressEvent{}},
		{name: "SGR mouse", raw: "\x1b[<0;10;5M", event: uv.MouseClickEvent{}},
		{name: "key while the color scheme is pending", inherit: true, raw: "a", event: uv.KeyPressEvent{}},
		{name: "settled OSC 11", inherit: true, settle: "\x1b]11;#203040\x07", raw: "\x1b]11;#203040\x07", event: uv.BackgroundColorEvent{}},
		{name: "settled OSC 4", inherit: true, settle: "\x1b]4;1;#123456\x07", raw: "\x1b]4;1;#123456;1;#654321\x07", event: uv.UnknownOscEvent("")},
		{name: "unsolicited OSC 10", raw: "\x1b]10;#203040\x07", event: uv.ForegroundColorEvent{}},
		{name: "unsolicited OSC 4", raw: "\x1b]4;1;#123456\x07", event: uv.UnknownOscEvent("")},
		{name: "out-of-range DECRPSS cursor style", raw: "\x1bP1$r9 q\x1b\\", event: uv.UnknownDcsEvent("")},
		// Go converts at most 32 bytes to a string on the stack.
		{name: "DECRPSS payload over 32 bytes", raw: "\x1bP2$r" + strings.Repeat("a", 40) + "\x1b\\", event: uv.UnknownDcsEvent("")},
		{name: "declined DECRPSS payload over 32 bytes", raw: "\x1bP0$r" + strings.Repeat("a", 40) + "\x1b\\", event: uv.UnknownDcsEvent("")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			packet := decodePacket(t, tt.raw)
			if reflect.TypeOf(packet.Event) != reflect.TypeOf(tt.event) {
				t.Fatalf("packet %q decoded as %T, want %T", tt.raw, packet.Event, tt.event)
			}
			c := &console{inherit: tt.inherit, pending: make(map[ansi.DECMode]bool)}
			c.preferenceQueries()
			if tt.settle != "" && !preferenceReply(t, c, tt.settle) {
				t.Fatalf("settling reply %q was not consumed", tt.settle)
			}
			c.consumeReply(packet)
			if allocs := testing.AllocsPerRun(100, func() {
				if c.consumeReply(packet) {
					t.Fatalf("%s packet was consumed as a reply", tt.name)
				}
			}); allocs != 0 {
				t.Fatalf("parsing a %s packet allocated %.0f times", tt.name, allocs)
			}
			if !c.preferences.cursorPending || !c.preferences.colors[12] || c.preferences.schemePending != tt.inherit {
				t.Fatal("cursor and color-scheme queries must stay pending so every run parses the packet")
			}
		})
	}
}

func TestPreferenceParsingKeepsPooledParserCapacity(t *testing.T) {
	c := &console{}
	c.preferenceQueries()
	packet := decodePacket(t, "a")
	uri := "https://example.com/" + strings.Repeat("hyperlink/", 8)
	text := ansi.SetHyperlink(uri) + "link" + ansi.ResetHyperlink()
	// The race detector drops a quarter of sync.Pool puts, so repeat the cycle
	// until a parser put back after preference parsing is handed to drawing.
	for i := range 32 {
		c.consumePreferenceString(packet)
		buf := uv.NewScreenBuffer(4, 1)
		uv.NewStyledString(text).Draw(buf, buf.Bounds())
		if got := buf.CellAt(0, 0).Link.URL; got != uri {
			t.Fatalf("cycle %d: hyperlink drawn after preference parsing = %q, want %q", i, got, uri)
		}
	}
}

func TestPreferenceRepliesRespectReplyParserBuffer(t *testing.T) {
	t.Run("whole palette at full precision", func(t *testing.T) {
		c := &console{inherit: true, pending: make(map[ansi.DECMode]bool)}
		c.preferenceQueries()
		want := make(map[uint8]ghostty.ColorRGB, ghostty.PaletteSize)
		var reply strings.Builder
		reply.WriteString("\x1b]4")
		// Three-digit indices make this the longest payload the buffer admits.
		for i := range ghostty.PaletteSize {
			rgb := ghostty.ColorRGB{R: uint8(i), G: uint8(255 - i), B: uint8(i * 7)}
			want[uint8(i)] = rgb
			fmt.Fprintf(&reply, ";%03d;rgb:%02x%02x/%02x%02x/%02x%02x", i, rgb.R, rgb.R, rgb.G, rgb.G, rgb.B, rgb.B)
		}
		reply.WriteString("\x07")
		if !preferenceReply(t, c, reply.String()) {
			t.Fatal("whole-palette reply was not consumed")
		}
		if len(c.preferences.palette) != 0 {
			t.Fatalf("%d palette entries are still pending", len(c.preferences.palette))
		}
		if !maps.Equal(c.preferences.profile.Palette, want) {
			t.Fatalf("decoded palette = %v, want %v", c.preferences.profile.Palette, want)
		}
	})
	t.Run("OSC payload past the buffer", func(t *testing.T) {
		c := &console{inherit: true, pending: make(map[ansi.DECMode]bool)}
		c.preferenceQueries()
		// Cutting the payload at the buffer leaves "#123", a valid color
		// that differs from the reported #123456.
		padding := strings.Repeat(" ", replyDataSize-len("11;#123"))
		if preferenceReply(t, c, "\x1b]11;"+padding+"#123456\x07") {
			t.Errorf("reply truncated by the parser buffer was consumed with background %+v", c.preferences.profile.Background)
		}
		if !c.preferences.colors[11] || c.preferences.profile.Background != nil {
			t.Fatalf("truncated reply settled the background query: pending=%v background=%+v", c.preferences.colors[11], c.preferences.profile.Background)
		}
	})
	t.Run("DCS payload past the buffer", func(t *testing.T) {
		c := &console{}
		c.preferenceQueries()
		// Cutting the payload at the buffer leaves a " q" suffix, which reads
		// as the terminal declining the cursor-style query.
		payload := strings.Repeat("a", replyDataSize-len(" q")) + " q" + "zz"
		if preferenceReply(t, c, "\x1bP0$r"+payload+"\x1b\\") {
			t.Error("DECRPSS reply truncated by the parser buffer was consumed")
		}
		if !c.preferences.cursorPending {
			t.Fatal("truncated DECRPSS reply settled the cursor-style query")
		}
	})
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
