package emulator

import (
	"errors"
	"fmt"
	"image/color"
	"slices"
	"unicode/utf8"

	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
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
	dirty       []bool
}

func (t *Terminal) State() (State, error) {
	var s State
	if err := t.UpdateState(&s); err != nil {
		return State{}, err
	}
	// UpdateState created these maps and optional values for this caller;
	// only the visual arrays borrow terminal storage.
	s.Cells = slices.Clone(s.Cells)
	s.NativeCells = slices.Clone(s.NativeCells)
	return s, nil
}

// UpdateState borrows the terminal's display storage and reuses dst's maps.
// The owner must serialize use and clone the result before publishing it.
// Display cells remain valid only until the next capture, resize or close.
func (t *Terminal) UpdateState(s *State) error {
	if t == nil || t.native == nil {
		return ErrClosed
	}
	if !t.held {
		if err := t.captureVisual(); err != nil {
			return err
		}
	}
	s.Cells, s.NativeCells = t.visual.cells, t.visual.nativeCells
	s.Cursor, s.Colors = t.visual.cursor, t.visual.colors
	if err := t.InputState(s); err != nil {
		return err
	}
	return t.readMetadata(s)
}

// InputState observes current routing modes and hold state without capturing
// the viewport. Other fields are unchanged until UpdateState is called.
func (t *Terminal) InputState(s *State) error {
	if t == nil || t.native == nil {
		return ErrClosed
	}
	s.Size, s.Held = t.size, t.held
	if s.Modes == nil {
		s.Modes = make(map[ghostty.Mode]bool, len(knownModes))
	}
	if s.ModeErrors == nil {
		s.ModeErrors = make(map[ghostty.Mode]error)
	}
	clear(s.Modes)
	clear(s.ModeErrors)
	var err error
	s.ModifyOtherKeys2, err = t.modifyOtherKeys2()
	if err != nil {
		return err
	}
	if err := errors.Join(
		read("Kitty keyboard flags", &s.KittyKeyboardFlags, t.native.KittyKeyboardFlags),
		read("mouse tracking", &s.MouseTracking, t.native.MouseTracking),
	); err != nil {
		return err
	}
	for _, mode := range knownModes {
		value, err := t.native.Mode(mode)
		if err != nil {
			s.ModeErrors[mode] = err
			continue
		}
		s.Modes[mode] = value
	}
	return nil
}

// DirtyRows accumulates captured rows until the compositor acknowledges them.
// Reading a snapshot must not consume damage needed by a later repaint.
func (t *Terminal) DirtyRows() []bool { return t.visual.dirty }

func (t *Terminal) ClearDamage() { clear(t.visual.dirty) }

func (t *Terminal) readMetadata(s *State) error {
	var screen ghostty.TerminalScreen
	err := errors.Join(
		read("active screen", &screen, t.native.ActiveScreen),
		read("title", &s.Title, t.native.Title),
		read("directory", &s.Directory, t.native.Pwd),
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
	visual := t.visual
	count := t.size.Cols * t.size.Rows
	colorsChanged := visual.colors != *colors
	if len(visual.cells) != count || colorsChanged {
		// Colors are resolved into each UV cell; palette/default-color changes
		// therefore invalidate cached styles even without any text damage.
		if err := t.render.SetDirty(ghostty.RenderStateDirtyFull); err != nil {
			return fmt.Errorf("invalidating native render styles: %w", err)
		}
	}
	if len(visual.cells) != count {
		visual.cells = slices.Grow(visual.cells, max(0, count-len(visual.cells)))[:count]
		visual.nativeCells = slices.Grow(visual.nativeCells, max(0, count-len(visual.nativeCells)))[:count]
	}
	if len(visual.dirty) != t.size.Rows {
		visual.dirty = slices.Grow(visual.dirty, max(0, t.size.Rows-len(visual.dirty)))[:t.size.Rows]
	}
	if colorsChanged {
		t.styleValid = false
	}
	visual.cursor, visual.colors = *cursor, *colors
	if err := t.render.RowIterator(t.rows); err != nil {
		return fmt.Errorf("reading native render rows: %w", err)
	}
	for {
		if _, ok := t.rows.NextDirty(); !ok {
			break
		}
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
		visual.dirty[int(y)] = true
	}
	if err := t.render.Clean(); err != nil {
		return fmt.Errorf("consuming native render damage: %w", err)
	}
	t.visual = visual
	return nil
}

func (t *Terminal) copyRow(y int, visual *visualState) error {
	raw, err := t.rows.CellsRaw()
	if err != nil {
		return fmt.Errorf("reading native packed cells: %w", err)
	}
	if raw.Len() != t.size.Cols {
		return fmt.Errorf("native row has %d cells, expected %d", raw.Len(), t.size.Cols)
	}
	selection, err := t.rows.Selection()
	if err != nil {
		return fmt.Errorf("reading native row selection: %w", err)
	}
	t.plainStyle = capturedStyle{native: *t.defaultStyle, visual: t.cellStyle(t.defaultStyle, visual.colors)}
	// Style IDs belong to the source page and may be reused after mutations.
	// Reuse only within this captured row; retain map storage between rows.
	clear(t.styles)
	for x := range t.size.Cols {
		i := y*t.size.Cols + x
		// CellsRaw supplies copied packed values without a native getter per
		// cell. Decode properties from the linked library's layout manifest.
		visual.nativeCells[i].Raw = *raw.Cell(x)
		visual.nativeCells[i].Selected = selection != nil && x >= int(selection.StartX) && x <= int(selection.EndX)
		cell, native, err := t.copyCell(i, visual, t.layout.decode(visual.nativeCells[i].Raw.PackedValue()))
		if err != nil {
			return fmt.Errorf("copying native cell %d,%d: %w", x, y, err)
		}
		visual.cells[i], visual.nativeCells[i] = cell, native
	}
	return nil
}

func (t *Terminal) copyCell(i int, visual *visualState, data cellData) (uv.Cell, NativeCell, error) {
	native := &visual.nativeCells[i]
	raw := &native.Raw
	style, err := t.rowStyle(uint16(i%t.size.Cols), data.styleID, visual.colors)
	if err != nil {
		return uv.Cell{}, NativeCell{}, err
	}
	t.text = t.text[:0]
	if data.tag == ghostty.CellContentCodepointGrapheme {
		if err := t.cells.Select(uint16(i % t.size.Cols)); err != nil {
			return uv.Cell{}, NativeCell{}, err
		}
		t.text, err = t.cells.AppendGraphemes(t.text)
		if err != nil {
			return uv.Cell{}, NativeCell{}, err
		}
	} else if data.codepoint != 0 {
		t.text = utf8.AppendRune(t.text, rune(data.codepoint))
	}
	cell := uv.Cell{Content: cellText(t.text, visual.cells[i].Content), Style: style.visual, Width: 1}
	// Erased cells store their background in the content union rather than
	// the style. Resolve it independently of the row's shared style cache.
	switch data.tag {
	case ghostty.CellContentBgColorPalette:
		cell.Style.Bg = t.paletteColor(&visual.colors.Palette, data.palette)
	case ghostty.CellContentBgColorRGB:
		cell.Style.Bg = rgbColor(data.rgb)
	}
	switch data.wide {
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
	if data.linked {
		x, y := i%t.size.Cols, i/t.size.Cols
		ref, err := t.native.GridRef(ghostty.Point{Tag: ghostty.PointTagViewport, X: uint16(x), Y: uint32(y)})
		if err != nil {
			return uv.Cell{}, NativeCell{}, err
		}
		cell.Link.URL, err = ref.HyperlinkURI()
		if err != nil {
			return uv.Cell{}, NativeCell{}, err
		}
	}
	return cell, NativeCell{Raw: *raw, Style: style.native, Wide: data.wide, Selected: native.Selected}, nil
}

// The immutable ASCII string supplies stable single-character strings without
// allocating; other unchanged graphemes retain their previous owned string.
const printableASCII = " !\"#$%&'()*+,-./0123456789:;<=>?@ABCDEFGHIJKLMNOPQRSTUVWXYZ[\\]^_`abcdefghijklmnopqrstuvwxyz{|}~"

func cellText(text []byte, previous string) string {
	if string(text) == previous {
		return previous
	}
	if len(text) == 1 && text[0] >= ' ' && text[0] <= '~' {
		i := int(text[0] - ' ')
		return printableASCII[i : i+1]
	}
	return string(text)
}

func (t *Terminal) cellStyle(style *ghostty.Style, colors ghostty.RenderStateColors) uv.Style {
	if !t.styleValid || t.style != *style {
		t.style, t.cachedStyle, t.styleValid = *style, t.uvStyle(style, colors), true
	}
	return t.cachedStyle
}

func (t *Terminal) uvStyle(style *ghostty.Style, colors ghostty.RenderStateColors) uv.Style {
	result := uv.Style{
		Fg: t.styleColor(style.FgColor(), &colors.Palette), Bg: t.styleColor(style.BgColor(), &colors.Palette),
		UnderlineColor: t.styleColor(style.UnderlineColor(), &colors.Palette),
	}
	// Keep host-matching defaults as default rendition to preserve opacity
	// and physical-terminal default-color policies. OSC overrides and isolated
	// defaults require explicit colors so rendering agrees with child queries.
	if result.Fg == nil && (t.hostForeground == nil || *t.hostForeground != colors.Foreground) {
		result.Fg = rgbColor(colors.Foreground)
	}
	if result.Bg == nil && (t.hostBackground == nil || *t.hostBackground != colors.Background) {
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

func (t *Terminal) styleColor(value ghostty.StyleColor, palette *ghostty.Palette) color.Color {
	switch value.Tag {
	case ghostty.StyleColorRGB:
		return rgbColor(value.RGB)
	case ghostty.StyleColorPalette:
		return t.paletteColor(palette, value.Palette)
	default:
		return nil
	}
}

// paletteColor keeps an entry the host reported, and the child has not
// changed, as a palette reference where the outer renderer would otherwise
// downsample it. The outer terminal then paints its own theme color instead of
// the nearest 256- or 16-color approximation. Child overrides and unreported
// entries stay explicit, so rendering agrees with the child's palette queries.
func (t *Terminal) paletteColor(palette *ghostty.Palette, i uint8) color.Color {
	if int(i) >= t.indexedEntries || !t.hostPaletteReported[i] || palette[i] != t.hostPalette[i] {
		return rgbColor(palette[i])
	}
	if i <= uint8(ansi.BrightWhite) {
		return ansi.BasicColor(i)
	}
	return ansi.IndexedColor(i)
}

// indexedPaletteEntries is how many leading palette entries the outer
// renderer passes through as indexes without losing the host's colors.
// ANSI256 passes every index. ANSI passes the 16 basic colors and maps higher
// indexes through a fixed table that ignores the host's RGB, which the RGB
// approximation preserves. Colorless profiles gain nothing, and a true-color
// renderer keeps the resolved RGB: Ultraviolet compares colors only by RGBA
// (charmbracelet/ultraviolet#205), so an index beside or replacing a color
// with its xterm default value would keep the earlier color.
func indexedPaletteEntries(p colorprofile.Profile) int {
	switch p {
	case colorprofile.ANSI256:
		return ghostty.PaletteSize
	case colorprofile.ANSI:
		return int(ansi.BrightWhite) + 1
	default:
		return 0
	}
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
