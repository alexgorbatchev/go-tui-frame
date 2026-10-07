package frame

import (
	"bytes"
	"fmt"
	"strconv"

	"github.com/alexgorbatchev/go-tui-frame/v2/internal/emulator"
	"github.com/alexgorbatchev/go-tui-frame/v2/internal/input"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	ghostty "go.mitchellh.com/libghostty"
)

const cursorStyleQuery = "\x1bP$q q\x1b\\"

// cursorStyleSuffix ends a DECRPSS cursor-style report: the DECSCUSR
// intermediate and final bytes after the reported style.
const cursorStyleSuffix = " q"
const colorSchemeQuery = "\x1b[?996n"

// oscSeparator separates OSC parameters.
const oscSeparator = ";"

// longestReportedColor is the longest color form terminals put in color
// reports: XParseColor rgb: with four hex digits per channel, which is how
// Ghostty encodes them.
const longestReportedColor = "rgb:ffff/ffff/ffff"

// replyDataSize bounds the OSC and DCS payload the reply parser collects.
// Each palette entry is queried on its own, so a real reply is short, at most
// 4;255;rgb:ffff/ffff/ffff. The bound is instead the longest reply
// consumeColor accepts in the form terminals emit: one OSC 4 that reports all
// 256 entries with three-digit indices. The accepted color syntax itself has
// no maximum length, because it allows padding. The parser drops payload bytes
// past its buffer without reporting them, so the buffer holds one byte more
// than that reply, and a payload that fills it may have been cut short.
const replyDataSize = len("4") + ghostty.PaletteSize*len(";255;"+longestReportedColor) + 1

type terminalPreferences struct {
	profile                              emulator.Profile
	colors                               map[int]bool
	palette                              map[uint8]bool
	modes                                map[ghostty.Mode]bool
	cursorPending, schemePending, closed bool
	cursorStyle                          *int
	// The reply parser belongs to the console instead of the ansi pool:
	// resizing a pooled parser shrinks the buffer that later pool users, such
	// as Ultraviolet's styled-string drawing, collect OSC 8 hyperlinks into.
	parser *ansi.Parser
	// consumed records whether the packet being parsed held a solicited reply,
	// so the handlers installed once on parser need no per-packet closures.
	consumed bool
}

func (p *terminalPreferences) pending() bool {
	return len(p.colors) > 0 || len(p.palette) > 0 || len(p.modes) > 0 || p.cursorPending || p.schemePending
}

func (c *console) preferenceQueries() string {
	p := &c.preferences
	p.parser = ansi.NewParser()
	p.parser.SetDataSize(replyDataSize)
	p.parser.SetHandler(ansi.Handler{HandleOsc: p.handleOsc, HandleDcs: p.handleDcs})
	p.colors = map[int]bool{12: true}
	p.cursorPending = true
	query := ansi.RequestCursorColor + cursorStyleQuery
	if !c.inherit {
		return query
	}
	p.colors[10], p.colors[11] = true, true
	p.palette = make(map[uint8]bool, ghostty.PaletteSize)
	p.modes = make(map[ghostty.Mode]bool)
	p.profile.Palette = make(map[uint8]ghostty.ColorRGB)
	p.profile.Modes = make(map[ghostty.Mode]bool)
	query += ansi.RequestForegroundColor + ansi.RequestBackgroundColor
	for i := range ghostty.PaletteSize {
		p.palette[uint8(i)] = true
		query += fmt.Sprintf("\x1b]4;%d;?\x07", i)
	}
	for _, mode := range emulator.PreferenceModes() {
		p.modes[mode] = true
		if mode.ANSI() {
			query += ansi.RequestMode(ansi.ANSIMode(mode.Value()))
		} else if !c.pending[ansi.DECMode(mode.Value())] {
			query += ansi.RequestMode(ansi.DECMode(mode.Value()))
		}
	}
	p.schemePending = true
	return query + colorSchemeQuery
}

// consumePreferenceReply reports whether packet answers a pending preference
// query, and settles that query. Only packets the input decoder recognizes as
// reply-shaped reach the reply parser: OSC 10, 11 and 12 decode as color
// events, OSC 4 as an unknown OSC string and a DECRPSS reply as an unknown DCS
// string. Keys, mouse reports and paste are never parsed.
func (c *console) consumePreferenceReply(packet input.Packet) bool {
	p := &c.preferences
	var scheme ghostty.ColorScheme
	switch ev := packet.Event.(type) {
	case uv.ModeReportEvent:
		var mode ghostty.Mode
		switch m := ev.Mode.(type) {
		case ansi.DECMode:
			mode = ghostty.NewMode(uint16(m), false)
		case ansi.ANSIMode:
			mode = ghostty.NewMode(uint16(m), true)
		default:
			return false
		}
		if !p.modes[mode] {
			return false
		}
		delete(p.modes, mode)
		if !p.closed && (ev.Value.IsSet() || ev.Value.IsReset()) {
			p.profile.Modes[mode] = ev.Value.IsSet()
		}
		return true
	case uv.DarkColorSchemeEvent:
		scheme = ghostty.ColorSchemeDark
	case uv.LightColorSchemeEvent:
		scheme = ghostty.ColorSchemeLight
	case uv.ForegroundColorEvent, uv.BackgroundColorEvent, uv.CursorColorEvent, uv.UnknownOscEvent, uv.UnknownDcsEvent:
		return c.consumePreferenceString(packet)
	default:
		return false
	}
	if !p.schemePending {
		return false
	}
	p.schemePending = false
	if !p.closed {
		p.profile.Scheme = new(scheme)
	}
	return true
}

func (c *console) consumePreferenceString(packet input.Packet) bool {
	p := &c.preferences
	if len(p.colors) == 0 && len(p.palette) == 0 && !p.cursorPending {
		return false
	}
	p.consumed = false
	p.parser.Reset()
	for _, b := range packet.Raw {
		p.parser.Advance(b)
	}
	return p.consumed
}

func (p *terminalPreferences) handleOsc(cmd int, data []byte) {
	// consumeColor settles the query whose report it accepts, so a later OSC
	// string in the same packet must not clear the result.
	if !truncatedReply(data) && p.consumeColor(cmd, data) {
		p.consumed = true
	}
}

func (p *terminalPreferences) handleDcs(cmd ansi.Cmd, params ansi.Params, data []byte) {
	valid, _, _ := params.Param(0, 0)
	if !p.cursorPending || cmd != ansi.Cmd(ansi.Command(0, '$', 'r')) || truncatedReply(data) {
		return
	}
	if valid == 0 && (len(data) == 0 || bytes.HasSuffix(data, []byte(cursorStyleSuffix))) {
		p.cursorPending = false
		p.consumed = true
		return
	}
	if !bytes.HasSuffix(data, []byte(cursorStyleSuffix)) {
		return
	}
	style, err := strconv.Atoi(string(bytes.TrimSuffix(data, []byte(cursorStyleSuffix))))
	if valid != 1 || err != nil || style < 1 || style > 6 {
		return
	}
	p.cursorPending, p.consumed = false, true
	if !p.closed {
		p.cursorStyle = new(style)
	}
}

// truncatedReply reports whether a payload filled the reply parser's buffer
// and may therefore have lost bytes the parser dropped.
func truncatedReply(data []byte) bool {
	return len(data) >= replyDataSize
}

// consumeColor reads the parser's buffer in place. A color is converted to a
// string for Ghostty only while its query is pending, so settled and
// unsolicited reports do not allocate.
func (p *terminalPreferences) consumeColor(cmd int, data []byte) bool {
	_, values, ok := bytes.Cut(data, []byte(oscSeparator))
	if !ok {
		return false
	}
	if cmd == 4 {
		return p.consumePalette(values)
	}
	if !p.colors[cmd] {
		return false
	}
	rgb, err := ghostty.ParseColor(string(values))
	if err != nil {
		return false
	}
	delete(p.colors, cmd)
	if !p.closed {
		switch cmd {
		case 10:
			p.profile.Foreground = new(rgb)
		case 11:
			p.profile.Background = new(rgb)
		case 12:
			p.profile.Cursor = new(rgb)
		}
	}
	return true
}

func (p *terminalPreferences) consumePalette(values []byte) bool {
	// OSC 4 reports index;color pairs, so an even separator count leaves an
	// index without a color.
	separators := bytes.Count(values, []byte(oscSeparator))
	if separators%2 == 0 || len(p.palette) == 0 {
		return false
	}
	consumed := false
	for range (separators + 1) / 2 {
		var field, color []byte
		field, values, _ = bytes.Cut(values, []byte(oscSeparator))
		color, values, _ = bytes.Cut(values, []byte(oscSeparator))
		index, err := strconv.ParseUint(string(field), 10, 8)
		if err != nil || !p.palette[uint8(index)] {
			continue
		}
		rgb, err := ghostty.ParseColor(string(color))
		if err != nil {
			continue
		}
		delete(p.palette, uint8(index))
		if !p.closed {
			p.profile.Palette[uint8(index)] = rgb
		}
		consumed = true
	}
	return consumed
}

func (c *console) childOptions(size emulator.Size) emulator.Options {
	opts := emulator.Options{Size: size, TerminfoName: endpointTerminfo, GraphemeWidth: c.graphemeWidth(), OuterColorProfile: c.colorProfile}
	if !c.inherit {
		return opts
	}
	p := &c.preferences.profile
	if c.preferences.cursorStyle != nil {
		n := *c.preferences.cursorStyle
		styles := []ghostty.TerminalCursorStyle{ghostty.TerminalCursorStyleBlock, ghostty.TerminalCursorStyleUnderline, ghostty.TerminalCursorStyleBar}
		style, blink := styles[(n-1)/2], n%2 == 1
		p.CursorStyle, p.CursorBlink = &style, &blink
	}
	opts.Profile = p
	return opts
}
