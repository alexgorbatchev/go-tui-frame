package emulator

import (
	"errors"
	"fmt"
	"image/color"
	"slices"

	uv "github.com/charmbracelet/ultraviolet"
	ghostty "go.mitchellh.com/libghostty"
)

// NativeCell preserves copied native information beyond UV's rendering fields.
// In particular, Style retains overline and Raw retains semantic content.
type NativeCell struct {
	Raw      ghostty.Cell
	Style    ghostty.Style
	Wide     ghostty.CellWide
	Selected bool
}

// State owns all display cells, maps and copied native values. While Held is
// true, Cells/Cursor/Colors describe the preserved complete frame; the remaining
// fields report the current native terminal state.
type State struct {
	Size               Size
	Cells              []uv.Cell
	NativeCells        []NativeCell
	Cursor             ghostty.RenderStateCursor
	Colors             ghostty.RenderStateColors
	Title, Directory   string
	Alternate, Held    bool
	Modes              map[ghostty.Mode]bool
	ModeErrors         map[ghostty.Mode]error
	KittyKeyboardFlags ghostty.KittyKeyFlags
	// ModifyOtherKeys2 is derived by the native encoder because the pinned
	// terminal getter API does not expose its internal modify_other_keys_2 bit.
	ModifyOtherKeys2                            bool
	MouseTracking                               bool
	MouseShape                                  ghostty.MouseShape
	Scrollbar                                   ghostty.Scrollbar
	ScrollbackRows, TotalRows                   uint
	ScrollbackMaxBytes, ScrollbackMaxLines      *uint
	MemoryUsage                                 ghostty.MemoryUsage
	VTGround, VTProcessingError, ViewportActive bool
	CursorPendingWrap, CursorAtPrompt           bool
}

type visualState struct {
	cells       []uv.Cell
	nativeCells []NativeCell
	cursor      ghostty.RenderStateCursor
	colors      ghostty.RenderStateColors
}

func (t *Terminal) State() (State, error) {
	if t == nil || t.native == nil {
		return State{}, ErrClosed
	}
	if !t.held {
		if err := t.captureVisual(); err != nil {
			return State{}, err
		}
	}
	s := State{
		Size: t.size, Cells: slices.Clone(t.visual.cells), NativeCells: slices.Clone(t.visual.nativeCells),
		Cursor: t.visual.cursor, Colors: t.visual.colors, Held: t.held,
		Modes: make(map[ghostty.Mode]bool, len(knownModes)), ModeErrors: make(map[ghostty.Mode]error),
	}
	if err := t.readMetadata(&s); err != nil {
		return State{}, err
	}
	var err error
	s.ModifyOtherKeys2, err = t.modifyOtherKeys2()
	if err != nil {
		return State{}, err
	}
	for _, mode := range knownModes {
		value, err := t.native.Mode(mode)
		if err != nil {
			s.ModeErrors[mode] = err
			continue
		}
		s.Modes[mode] = value
	}
	return s, nil
}

func (t *Terminal) readMetadata(s *State) error {
	var screen ghostty.TerminalScreen
	err := errors.Join(
		read("active screen", &screen, t.native.ActiveScreen),
		read("title", &s.Title, t.native.Title),
		read("directory", &s.Directory, t.native.Pwd),
		read("Kitty keyboard flags", &s.KittyKeyboardFlags, t.native.KittyKeyboardFlags),
		read("mouse tracking", &s.MouseTracking, t.native.MouseTracking),
		read("mouse shape", &s.MouseShape, t.native.MouseShape),
		read("scrollbar", &s.Scrollbar, t.native.Scrollbar),
		read("scrollback rows", &s.ScrollbackRows, t.native.ScrollbackRows),
		read("total rows", &s.TotalRows, t.native.TotalRows),
		read("scrollback byte limit", &s.ScrollbackMaxBytes, t.native.ScrollbackMaxBytes),
		read("scrollback line limit", &s.ScrollbackMaxLines, t.native.ScrollbackMaxLines),
		read("memory usage", &s.MemoryUsage, t.native.MemoryUsage),
		read("VT ground state", &s.VTGround, t.native.VTGround),
		read("VT processing status", &s.VTProcessingError, t.native.VTProcessingError),
		read("active viewport", &s.ViewportActive, t.native.ViewportActive),
		read("pending cursor wrap", &s.CursorPendingWrap, t.native.CursorPendingWrap),
		read("cursor prompt", &s.CursorAtPrompt, t.native.CursorAtPrompt),
	)
	s.Alternate = screen == ghostty.ScreenAlternate
	return err
}

func read[T any](name string, dst *T, getter func() (T, error)) error {
	value, err := getter()
	if err != nil {
		return fmt.Errorf("reading native %s: %w", name, err)
	}
	*dst = value
	return nil
}

func (t *Terminal) captureVisual() error {
	if err := t.checkGeometry(); err != nil {
		return err
	}
	if err := t.render.Update(t.native); err != nil {
		return fmt.Errorf("updating native render state: %w", err)
	}
	cursor, err := t.render.Cursor()
	if err != nil {
		return fmt.Errorf("reading native render cursor: %w", err)
	}
	colors, err := t.render.Colors()
	if err != nil {
		return fmt.Errorf("reading native render colors: %w", err)
	}
	visual := visualState{
		cells:       make([]uv.Cell, t.size.Cols*t.size.Rows),
		nativeCells: make([]NativeCell, t.size.Cols*t.size.Rows),
		cursor:      *cursor, colors: *colors,
	}
	if err := t.render.RowIterator(t.rows); err != nil {
		return fmt.Errorf("reading native render rows: %w", err)
	}
	for t.rows.Next() {
		y, err := t.rows.ViewportY()
		if err != nil {
			return fmt.Errorf("reading native row position: %w", err)
		}
		if y < 0 || int(y) >= t.size.Rows {
			continue
		}
		if err := t.rows.Cells(t.cells); err != nil {
			return fmt.Errorf("reading native row cells: %w", err)
		}
		if err := t.copyRow(int(y), &visual); err != nil {
			return err
		}
	}
	t.visual = visual
	return nil
}

func (t *Terminal) copyRow(y int, visual *visualState) error {
	var text []byte
	for x := range t.size.Cols {
		if err := t.cells.Select(uint16(x)); err != nil {
			return fmt.Errorf("selecting native cell %d,%d: %w", x, y, err)
		}
		cell, native, err := t.copyCell(x, y, visual.colors, &text)
		if err != nil {
			return fmt.Errorf("copying native cell %d,%d: %w", x, y, err)
		}
		i := y*t.size.Cols + x
		visual.cells[i], visual.nativeCells[i] = cell, native
	}
	return nil
}

func (t *Terminal) copyCell(x, y int, colors ghostty.RenderStateColors, text *[]byte) (uv.Cell, NativeCell, error) {
	raw, err := t.cells.Raw()
	if err != nil {
		return uv.Cell{}, NativeCell{}, err
	}
	style, err := t.cells.Style()
	if err != nil {
		return uv.Cell{}, NativeCell{}, err
	}
	wide, err := raw.Wide()
	if err != nil {
		return uv.Cell{}, NativeCell{}, err
	}
	selected, err := t.cells.Selected()
	if err != nil {
		return uv.Cell{}, NativeCell{}, err
	}
	*text, err = t.cells.AppendGraphemes((*text)[:0])
	if err != nil {
		return uv.Cell{}, NativeCell{}, err
	}
	cell := uv.Cell{Content: string(*text), Style: uvStyle(style, colors), Width: 1}
	switch wide {
	case ghostty.CellWideWide:
		cell.Width = 2
	case ghostty.CellWideSpacerTail:
		cell.Content, cell.Width = "", 0
	case ghostty.CellWideSpacerHead:
		// This physical cell precedes a soft-wrapped wide character. It
		// remains a blank cell rather than a tail of a nonexistent glyph.
		cell.Content = " "
	}
	if cell.Content == "" && cell.Width > 0 {
		cell.Content = " "
	}
	linked, err := raw.HasHyperlink()
	if err != nil {
		return uv.Cell{}, NativeCell{}, err
	}
	if linked {
		ref, err := t.native.GridRef(ghostty.Point{Tag: ghostty.PointTagViewport, X: uint16(x), Y: uint32(y)})
		if err != nil {
			return uv.Cell{}, NativeCell{}, err
		}
		cell.Link.URL, err = ref.HyperlinkURI()
		if err != nil {
			return uv.Cell{}, NativeCell{}, err
		}
	}
	return cell, NativeCell{Raw: *raw, Style: *style, Wide: wide, Selected: selected}, nil
}

func uvStyle(style *ghostty.Style, colors ghostty.RenderStateColors) uv.Style {
	result := uv.Style{
		Fg: styleColor(style.FgColor(), colors.Palette), Bg: styleColor(style.BgColor(), colors.Palette),
		UnderlineColor: styleColor(style.UnderlineColor(), colors.Palette),
	}
	// Explicit default colors are required: the child can change OSC 10/11
	// without changing any cell's StyleColorNone, and the physical terminal's
	// defaults may differ from the virtual terminal's queried defaults.
	if result.Fg == nil {
		result.Fg = rgbColor(colors.Foreground)
	}
	if result.Bg == nil {
		result.Bg = rgbColor(colors.Background)
	}
	flags := []struct {
		enabled bool
		attr    uint8
	}{
		{style.Bold(), uv.AttrBold}, {style.Faint(), uv.AttrFaint}, {style.Italic(), uv.AttrItalic},
		{style.Blink(), uv.AttrBlink}, {style.Inverse(), uv.AttrReverse},
		{style.Invisible(), uv.AttrConceal}, {style.Strikethrough(), uv.AttrStrikethrough},
	}
	for _, flag := range flags {
		if flag.enabled {
			result.Attrs |= flag.attr
		}
	}
	switch style.Underline() {
	case ghostty.UnderlineSingle:
		result.Underline = uv.UnderlineSingle
	case ghostty.UnderlineDouble:
		result.Underline = uv.UnderlineDouble
	case ghostty.UnderlineCurly:
		result.Underline = uv.UnderlineCurly
	case ghostty.UnderlineDotted:
		result.Underline = uv.UnderlineDotted
	case ghostty.UnderlineDashed:
		result.Underline = uv.UnderlineDashed
	}
	return result
}

func styleColor(value ghostty.StyleColor, palette ghostty.Palette) color.Color {
	var rgb ghostty.ColorRGB
	switch value.Tag {
	case ghostty.StyleColorRGB:
		rgb = value.RGB
	case ghostty.StyleColorPalette:
		rgb = palette[value.Palette]
	default:
		return nil
	}
	return rgbColor(rgb)
}

func rgbColor(rgb ghostty.ColorRGB) color.RGBA {
	return color.RGBA{R: rgb.R, G: rgb.G, B: rgb.B, A: 255}
}

// This is the binding's explicit set of named modes. A failed getter is kept in
// ModeErrors; it is never converted into an invented disabled value.
var knownModes = []ghostty.Mode{
	ghostty.ModeKAM, ghostty.ModeInsert, ghostty.ModeSRM, ghostty.ModeLinefeed,
	ghostty.ModeDECCKM, ghostty.Mode132Column, ghostty.ModeSlowScroll, ghostty.ModeReverseColors,
	ghostty.ModeOrigin, ghostty.ModeWraparound, ghostty.ModeAutorepeat, ghostty.ModeX10Mouse,
	ghostty.ModeCursorBlinking, ghostty.ModeCursorVisible, ghostty.ModeEnableMode3, ghostty.ModeReverseWrap,
	ghostty.ModeAltScreenLegacy, ghostty.ModeKeypadKeys, ghostty.ModeBackarrowKeyMode, ghostty.ModeLeftRightMargin,
	ghostty.ModeNormalMouse, ghostty.ModeButtonMouse, ghostty.ModeAnyMouse, ghostty.ModeFocusEvent,
	ghostty.ModeUTF8Mouse, ghostty.ModeSGRMouse, ghostty.ModeAltScroll, ghostty.ModeURxvtMouse,
	ghostty.ModeSGRPixelsMouse, ghostty.ModeNumlockKeypad, ghostty.ModeAltEscPrefix, ghostty.ModeAltSendsEsc,
	ghostty.ModeReverseWrapExt, ghostty.ModeAltScreen, ghostty.ModeSaveCursor, ghostty.ModeAltScreenSave,
	ghostty.ModeBracketedPaste, ghostty.ModeSyncOutput, ghostty.ModeGraphemeCluster, ghostty.ModeColorSchemeReport,
	ghostty.ModeVisibilityReport, ghostty.ModeInBandResize, ghostty.ModePasteEvents,
}
