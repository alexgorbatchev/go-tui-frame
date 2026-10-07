package emulator

import (
	uv "github.com/charmbracelet/ultraviolet"
	ghostty "go.mitchellh.com/libghostty"
)

type capturedStyle struct {
	native ghostty.Style
	visual uv.Style
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
	t.rowStyles = append(t.rowStyles, rowStyleEntry{id: id, style: capturedStyle{native: *style, visual: t.uvStyle(style, colors)}})
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
