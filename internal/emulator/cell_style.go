package emulator

import (
	"slices"

	uv "github.com/charmbracelet/ultraviolet"
	ghostty "go.mitchellh.com/libghostty"
)

// capturedStyle is a native style's position in the current row's style
// section and its conversion to UV. Position 0 is the default style, which
// belongs to no section; position p > 0 is the section's entry p-1.
type capturedStyle struct {
	index  uint32
	visual uv.Style
}

// defaultStyleIndex is the default style's index in every style table, so a
// zero NativeCell resolves to the default style.
const defaultStyleIndex = 0

// styleSpan locates one row's section in a style table.
type styleSpan struct{ start, len uint32 }

// The style table that UpdateState lends holds the default style at index 0
// and then one section per viewport row, in row order. A row's section holds
// each style the row's cells use once, found by native style ID while the row
// is converted, so no style value is hashed. A row of N cells has at most N
// styles, so the table never holds more than one style per cell plus the
// default style. A style that several rows use is stored once per row.
//
// Clean rows keep their sections and their cells' indices. A conversion
// collects the sections of the rows it reads; assembleStyles then builds the
// next table from those and the clean rows' sections, and moves the indices
// of clean rows whose section moved.

// initStyles creates the style table holding only the default style.
func (t *Terminal) initStyles() {
	t.styles = []ghostty.Style{*t.defaultStyle}
}

// beginStyles prepares the section storage for a conversion of a viewport
// of rows rows. A geometry change makes the conversion read every row, so a
// resized row span's earlier value is never used.
func (t *Terminal) beginStyles(rows int) {
	t.convertedStyles = t.convertedStyles[:0]
	if len(t.rowSpans) != rows {
		t.rowSpans = slices.Grow(t.rowSpans[:0], rows)[:rows]
		t.convertedSpans = slices.Grow(t.convertedSpans[:0], rows)[:rows]
		t.convertedRows = slices.Grow(t.convertedRows[:0], rows)[:rows]
	}
	clear(t.convertedRows)
}

// endRowStyles records the section of row y, whose styles the conversion
// collected from start on.
func (t *Terminal) endRowStyles(y, start int) {
	t.convertedSpans[y] = styleSpan{start: uint32(start), len: uint32(len(t.convertedStyles) - start)}
	t.convertedRows[y] = true
}

// assembleStyles builds the table for cells, a complete conversion's
// viewport: the default style, then each row's section. Converted rows take
// their new sections, and their cells' section positions become table
// indices; clean rows keep their sections, and their indices move with them.
// The previous table becomes the spare storage of the next assembly.
func (t *Terminal) assembleStyles(cells []NativeCell) {
	if !slices.Contains(t.convertedRows, true) {
		return
	}
	next := append(t.spareStyles[:0], *t.defaultStyle)
	cols := t.size.Cols
	for y, old := range t.rowSpans {
		row := cells[y*cols : (y+1)*cols]
		start := uint32(len(next))
		if t.convertedRows[y] {
			span := t.convertedSpans[y]
			next = append(next, t.convertedStyles[span.start:span.start+span.len]...)
			moveStyleIndices(row, start-1)
			t.rowSpans[y] = styleSpan{start: start, len: span.len}
			continue
		}
		next = append(next, t.styles[old.start:old.start+old.len]...)
		if start != old.start {
			// Unsigned arithmetic wraps, so the delta also moves indices down.
			moveStyleIndices(row, start-old.start)
		}
		t.rowSpans[y].start = start
	}
	t.spareStyles, t.styles = t.styles, next
}

// moveStyleIndices adds delta to the style index of every cell of row that
// does not use the default style.
func moveStyleIndices(row []NativeCell, delta uint32) {
	for i := range row {
		if row[i].StyleIndex != defaultStyleIndex {
			row[i].StyleIndex += delta
		}
	}
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
	// Each row style entry is one entry of the row's section, in order.
	t.convertedStyles = append(t.convertedStyles, *style)
	index := uint32(len(t.rowStyles) + 1)
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
