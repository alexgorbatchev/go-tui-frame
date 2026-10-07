package emulator

import (
	"fmt"
	"math"
	"slices"

	uv "github.com/charmbracelet/ultraviolet"
	ghostty "go.mitchellh.com/libghostty"
)

// capturedStyle is a native style's index in the terminal's style table and
// its conversion to UV.
type capturedStyle struct {
	index  uint32
	visual uv.Style
}

// defaultStyleIndex is the default style's index in every style table, so a
// zero NativeCell resolves to the default style.
const defaultStyleIndex = 0

// initStyles creates the style table holding only the default style.
func (t *Terminal) initStyles() {
	t.styles = []ghostty.Style{*t.defaultStyle}
	t.styleIndex = map[ghostty.Style]uint32{*t.defaultStyle: defaultStyleIndex}
}

// compactStyles bounds the table that UpdateState lends at one style per cell
// plus the default style, so a snapshot of distinct styles is no larger than
// one whose cells each held a full style. Once a conversion leaves the table
// above that bound, it drops the styles no cell of cells references and
// renumbers the cells' indices in place, so clean rows need no recapture.
// Kept styles keep their order, and the default style keeps index 0.
func (t *Terminal) compactStyles(cells []NativeCell) {
	if len(t.styles) <= len(cells)+1 {
		return
	}
	// remap marks each referenced style, then holds its new index.
	remap := slices.Grow(t.styleRemap[:0], len(t.styles))[:len(t.styles)]
	clear(remap)
	remap[defaultStyleIndex] = 1
	for i := range cells {
		remap[cells[i].StyleIndex] = 1
	}
	kept := 0
	for i := range t.styles {
		if remap[i] == 0 {
			delete(t.styleIndex, t.styles[i])
			continue
		}
		remap[i] = uint32(kept)
		if kept != i {
			t.styles[kept] = t.styles[i]
			t.styleIndex[t.styles[kept]] = uint32(kept)
		}
		kept++
	}
	t.styles = t.styles[:kept]
	for i := range cells {
		cells[i].StyleIndex = remap[cells[i].StyleIndex]
	}
	t.styleRemap = remap
}

// internStyle returns style's index in the style table, appending it on first
// use. The table persists across captures because clean rows keep the indices
// an earlier capture assigned; native style IDs cannot serve as indices,
// because they belong to the source page and may be reused. Indices stay
// valid until compactStyles renumbers the cells that hold them.
func (t *Terminal) internStyle(style *ghostty.Style) (uint32, error) {
	if i, ok := t.styleIndex[*style]; ok {
		return i, nil
	}
	if uint64(len(t.styles)) > math.MaxUint32 {
		return 0, fmt.Errorf("native style table exceeds %d styles", uint64(math.MaxUint32)+1)
	}
	i := uint32(len(t.styles))
	t.styles = append(t.styles, *style)
	t.styleIndex[*style] = i
	return i, nil
}

// rowStyleScanLimit is how many of a row's styles rowStyle scans. Rows with
// more styles, such as gradients that color each cell, index them by ID
// instead, so a lookup does not scan every earlier style of the row.
const rowStyleScanLimit = 16

// rowStyleEntry maps a native style ID to its style within one captured row.
type rowStyleEntry struct {
	id    uint16
	style capturedStyle
}

// resetRowStyles forgets the previous row's style IDs. Style IDs belong to
// the source page and may be reused after mutations, so they identify a
// style only within one captured row. The storage is kept between rows.
func (t *Terminal) resetRowStyles() {
	t.rowStyles = t.rowStyles[:0]
	t.lastRowStyle = 0
	if len(t.rowStyleIndex) > 0 {
		clear(t.rowStyleIndex)
	}
}

// rowStyle returns the style of the cell at x of the current row, whose
// native style ID is id. The result is valid until the next call. Cells
// repeat styles in runs, so the previous cell's entry is checked first. A row
// with few styles is then scanned; a row with more is looked up by index.
func (t *Terminal) rowStyle(x, id uint16, colors *ghostty.RenderStateColors) (*capturedStyle, error) {
	// Native style ID zero denotes the default style.
	if id == 0 {
		return &t.plainStyle, nil
	}
	if t.lastRowStyle < len(t.rowStyles) && t.rowStyles[t.lastRowStyle].id == id {
		return &t.rowStyles[t.lastRowStyle].style, nil
	}
	if i, ok := t.findRowStyle(id); ok {
		t.lastRowStyle = i
		return &t.rowStyles[i].style, nil
	}
	if err := t.cells.Select(x); err != nil {
		return nil, err
	}
	style, err := t.cells.Style()
	if err != nil {
		return nil, err
	}
	index, err := t.internStyle(style)
	if err != nil {
		return nil, err
	}
	t.rowStyles = append(t.rowStyles, rowStyleEntry{id: id, style: capturedStyle{index: index, visual: t.uvStyle(style, colors)}})
	t.lastRowStyle = len(t.rowStyles) - 1
	t.indexRowStyles()
	return &t.rowStyles[t.lastRowStyle].style, nil
}

func (t *Terminal) findRowStyle(id uint16) (int, bool) {
	if len(t.rowStyleIndex) > 0 {
		i, ok := t.rowStyleIndex[id]
		return i, ok
	}
	for i := range t.rowStyles {
		if t.rowStyles[i].id == id {
			return i, true
		}
	}
	return 0, false
}

// indexRowStyles indexes the row's styles once there are more than
// rowStyleScanLimit, and then each style added after them.
func (t *Terminal) indexRowStyles() {
	switch n := len(t.rowStyles); {
	case n < rowStyleScanLimit+1:
	case n == rowStyleScanLimit+1:
		if t.rowStyleIndex == nil {
			t.rowStyleIndex = make(map[uint16]int)
		}
		for i := range t.rowStyles {
			t.rowStyleIndex[t.rowStyles[i].id] = i
		}
	default:
		t.rowStyleIndex[t.rowStyles[n-1].id] = n - 1
	}
}
