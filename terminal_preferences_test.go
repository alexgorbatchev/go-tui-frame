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
				t.Fatal("cursor and color-scheme queries must stay pending so every run reaches the reply checks")
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

// pastedReply frames reply inside a bracketed paste, as copied terminal output
// or a shell snippet holding a literal report arrives.
func pastedReply(reply string) string {
	return "\x1b[200~echo hi\n" + reply + "\n\x1b[201~"
}

// Bracketed-paste payload is user input even when it holds a well-formed
// report for a query the frame still waits on, during the probe or after it.
// The query stays pending for the terminal's own reply.
func TestPastedRepliesStayUserInput(t *testing.T) {
	for _, tt := range []struct {
		name, reply string
		inherit     bool
		pending     func(*terminalPreferences) bool
	}{
		{"cursor color", "\x1b]12;rgb:ffff/0000/0000\x07", false, func(p *terminalPreferences) bool { return p.colors[12] }},
		{"foreground color", "\x1b]10;rgb:aaaa/bbbb/cccc\x1b\\", true, func(p *terminalPreferences) bool { return p.colors[10] }},
		{"background color", "\x1b]11;#203040\x07", true, func(p *terminalPreferences) bool { return p.colors[11] }},
		{"palette color", "\x1b]4;1;#123456\x07", true, func(p *terminalPreferences) bool { return p.palette[1] }},
		{"cursor style", "\x1bP1$r4 q\x1b\\", false, func(p *terminalPreferences) bool { return p.cursorPending }},
	} {
		for _, phase := range []struct {
			name   string
			closed bool
		}{{"during the probe", false}, {"after the probe", true}} {
			t.Run(tt.name+" "+phase.name, func(t *testing.T) {
				c := &console{inherit: tt.inherit, pending: make(map[ansi.DECMode]bool)}
				c.preferenceQueries()
				c.preferences.closed = phase.closed
				packets, err := input.New().Feed([]byte(pastedReply(tt.reply)))
				if err != nil {
					t.Fatal(err)
				}
				if len(packets) != 3 || !packets[1].Paste || !strings.Contains(string(packets[1].Raw), tt.reply) {
					t.Fatalf("paste was not framed as one payload holding %q: %#v", tt.reply, packets)
				}
				for _, p := range packets {
					if c.consumeReply(p) {
						t.Errorf("paste packet %q was consumed as a reply", p.Raw)
					}
				}
				p := &c.preferences
				if !tt.pending(p) {
					t.Errorf("pasted %q settled its query", tt.reply)
				}
				if p.profile.Foreground != nil || p.profile.Background != nil || p.profile.Cursor != nil || len(p.profile.Palette) != 0 || p.cursorStyle != nil {
					t.Errorf("pasted %q changed the child's defaults", tt.reply)
				}
				if !preferenceReply(t, c, tt.reply) {
					t.Errorf("the terminal's own %q reply after the paste was not consumed", tt.reply)
				}
			})
		}
	}
}

// A paste routed after the probe, while the cursor color query is still
// pending, reaches the child whole although it holds that query's report.
func TestRoutedPasteKeepsPendingReplyText(t *testing.T) {
	s, _, slave := newRepaintSession(t, ansi.ModeReset, nil)
	router, err := newInputRouter(s.terminal, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(router.close)
	s.router = router
	// The child requests bracketed paste, so the markers reach it as well.
	readRepaintChunk(t, s, slave, "\x1b[?2004h")
	s.console.preferenceQueries()
	s.console.preferences.closed = true
	paste := pastedReply("\x1b]12;rgb:ffff/0000/0000\x07")
	packets, err := input.New().Feed([]byte(paste))
	if err != nil {
		t.Fatal(err)
	}
	queued := len(s.queue)
	for _, p := range packets {
		if err := s.route(p); err != nil {
			t.Fatal(err)
		}
	}
	var routed []byte
	for _, p := range s.queue[queued:] {
		routed = append(routed, p.bytes[p.offset:]...)
	}
	if string(routed) != paste {
		t.Fatalf("child input queue holds %q, want the whole paste %q", routed, paste)
	}
}

// consumeColor settles a query as it accepts the report, so a packet holding
// several OSC strings stays consumed once one of them was a solicited reply,
// whatever follows it.
func TestPreferenceStringStaysConsumedAfterLaterOSC(t *testing.T) {
	c := &console{inherit: true, pending: make(map[ansi.DECMode]bool)}
	c.preferenceQueries()
	packet := input.Packet{Raw: []byte("\x1b]10;rgb:aaaa/bbbb/cccc\x07\x1b]13;#123456\x07")}
	consumed := c.consumePreferenceString(packet)
	if c.preferences.colors[10] {
		t.Fatal("OSC 10 reply did not settle the foreground query")
	}
	if !consumed {
		t.Fatal("packet whose OSC 10 reply settled the foreground query was reported unconsumed")
	}
}

// Only packets the input decoder recognizes as reply-shaped reach the reply
// parser. A query the terminal never answers stays pending for the whole
// session, so keys, mouse and focus reports and paste must not each pay for a
// parse meanwhile.
func TestReplyParserSkipsPacketsThatCannotBeReplies(t *testing.T) {
	c := &console{inherit: true, pending: make(map[ansi.DECMode]bool)}
	c.preferenceQueries()
	c.preferences.closed = true
	packets, err := input.New().Feed([]byte("a\x1b[<0;10;5M\x1b[I" + pastedReply("\x1b]12;rgb:ffff/0000/0000\x07")))
	if err != nil {
		t.Fatal(err)
	}
	// The parser keeps the command of the last sequence it parsed, here an
	// unsolicited OSC 13 report, until it parses another packet.
	const parsed = 13
	for _, p := range packets {
		if preferenceReply(t, c, "\x1b]13;#123456\x07") {
			t.Fatal("unsolicited OSC 13 report was consumed")
		}
		if got := c.preferences.parser.Command(); got != parsed {
			t.Fatalf("reply parser command after an OSC 13 report = %d, want %d", got, parsed)
		}
		if c.consumeReply(p) {
			t.Errorf("%T packet %q was consumed as a reply", p.Event, p.Raw)
		}
		if got := c.preferences.parser.Command(); got != parsed {
			t.Errorf("%T packet %q reached the reply parser", p.Event, p.Raw)
		}
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

// Programs end a probe batch with DA1 and stop reading at its reply, so a
// missing reply stalls them until their own query timeout.
func TestChildDeviceAttributeQueriesReceiveNativeDefaults(t *testing.T) {
	for _, inherit := range []bool{false, true} {
		t.Run(fmt.Sprintf("inherit=%v", inherit), func(t *testing.T) {
			c := &console{inherit: inherit, pending: make(map[ansi.DECMode]bool)}
			c.preferenceQueries()
			em, err := emulator.New(c.childOptions(emulator.Size{Cols: 10, Rows: 3}))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(em.Close)
			for _, tt := range []struct{ name, query, reply string }{
				{"DA1", "\x1b[c", "\x1b[?62;22c"},
				{"DA2", "\x1b[>c", "\x1b[>1;0;0c"},
				{"DA3", "\x1b[=c", "\x1bP!|00000000\x1b\\"},
			} {
				t.Run(tt.name, func(t *testing.T) {
					if _, err := em.Write([]byte(tt.query)); err != nil {
						t.Fatal(err)
					}
					var got []string
					for _, effect := range em.Effects() {
						got = append(got, fmt.Sprintf("%s %q", effect.Kind, effect.Bytes))
					}
					want := fmt.Sprintf("%s %q", emulator.Reply, tt.reply)
					if len(got) != 1 || got[0] != want {
						t.Fatalf("query %q effects = %s, want exactly [%s]", tt.query, got, want)
					}
				})
			}
		})
	}
}
