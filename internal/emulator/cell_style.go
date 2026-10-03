package emulator

import (
	uv "github.com/charmbracelet/ultraviolet"
	ghostty "go.mitchellh.com/libghostty"
)

type capturedStyle struct {
	native ghostty.Style
	visual uv.Style
}

func (t *Terminal) rowStyle(x, id uint16, colors ghostty.RenderStateColors) (capturedStyle, error) {
	// Native style ID zero denotes the default style. Plain rows need no map.
	if id == 0 {
		return t.plainStyle, nil
	}
	if cached, ok := t.styles[id]; ok {
		return cached, nil
	}
	if err := t.cells.Select(x); err != nil {
		return capturedStyle{}, err
	}
	style, err := t.cells.Style()
	if err != nil {
		return capturedStyle{}, err
	}
	if t.styles == nil {
		t.styles = make(map[uint16]capturedStyle)
	}
	cached := capturedStyle{native: *style, visual: t.cellStyle(style, colors)}
	t.styles[id] = cached
	return cached, nil
}
