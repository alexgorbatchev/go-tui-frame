package frame

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/alexgorbatchev/go-tui-frame/internal/emulator"
	"github.com/alexgorbatchev/go-tui-frame/internal/input"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	ghostty "go.mitchellh.com/libghostty"
)

const cursorStyleQuery = "\x1bP$q q\x1b\\"
const colorSchemeQuery = "\x1b[?996n"

// longestReportedColor is the longest XParseColor rgb: form a terminal
// reports: four hex digits per channel.
const longestReportedColor = "rgb:ffff/ffff/ffff"

// replyDataSize bounds the OSC and DCS payload the reply parser collects. The
// longest solicited reply is an OSC 4 that reports the whole palette in one
// sequence. The parser drops payload bytes past its buffer without reporting
// them, so the buffer holds one byte more than that reply, and a payload that
// fills it may have been cut short.
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
	// consumed records whether the packet being parsed was a solicited reply,
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

func (c *console) consumePreferenceReply(packet input.Packet) bool {
	p := &c.preferences
	if ev, ok := packet.Event.(uv.ModeReportEvent); ok {
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
	}
	if p.schemePending {
		var scheme ghostty.ColorScheme
		switch packet.Event.(type) {
		case uv.DarkColorSchemeEvent:
			scheme = ghostty.ColorSchemeDark
		case uv.LightColorSchemeEvent:
			scheme = ghostty.ColorSchemeLight
		default:
			return c.consumePreferenceString(packet)
		}
		p.schemePending = false
		if !p.closed {
			p.profile.Scheme = &scheme
		}
		return true
	}
	return c.consumePreferenceString(packet)
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
	p.consumed = !truncatedReply(data) && p.consumeColor(cmd, string(data))
}

func (p *terminalPreferences) handleDcs(cmd ansi.Cmd, params ansi.Params, data []byte) {
	valid, _, _ := params.Param(0, 0)
	if !p.cursorPending || cmd != ansi.Cmd(ansi.Command(0, '$', 'r')) || truncatedReply(data) {
		return
	}
	if valid == 0 && (len(data) == 0 || strings.HasSuffix(string(data), " q")) {
		p.cursorPending = false
		p.consumed = true
		return
	}
	if !strings.HasSuffix(string(data), " q") {
		return
	}
	style, err := strconv.Atoi(strings.TrimSuffix(string(data), " q"))
	if valid != 1 || err != nil || style < 1 || style > 6 {
		return
	}
	p.cursorPending, p.consumed = false, true
	if !p.closed {
		p.cursorStyle = &style
	}
}

// truncatedReply reports whether a payload filled the reply parser's buffer
// and may therefore have lost bytes the parser dropped.
func truncatedReply(data []byte) bool {
	return len(data) >= replyDataSize
}

func (p *terminalPreferences) consumeColor(cmd int, data string) bool {
	_, values, ok := strings.Cut(data, ";")
	if !ok {
		return false
	}
	if cmd == 4 {
		parts := strings.Split(values, ";")
		if len(parts)%2 != 0 {
			return false
		}
		consumed := false
		for i := 0; i < len(parts); i += 2 {
			index, err := strconv.ParseUint(parts[i], 10, 8)
			if err != nil || !p.palette[uint8(index)] {
				continue
			}
			rgb, err := ghostty.ParseColor(parts[i+1])
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
	if !p.colors[cmd] {
		return false
	}
	rgb, err := ghostty.ParseColor(values)
	if err != nil {
		return false
	}
	delete(p.colors, cmd)
	if !p.closed {
		switch cmd {
		case 10:
			p.profile.Foreground = &rgb
		case 11:
			p.profile.Background = &rgb
		case 12:
			p.profile.Cursor = &rgb
		}
	}
	return true
}

func (c *console) childOptions(size emulator.Size) emulator.Options {
	opts := emulator.Options{Size: size, TerminfoName: "xterm-256color", GraphemeWidth: c.supports(2027)}
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
