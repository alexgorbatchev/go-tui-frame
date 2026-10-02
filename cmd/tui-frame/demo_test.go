package main

import (
	"image/color"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	frame "github.com/alexgorbatchev/go-tui-frame"
	uv "github.com/charmbracelet/ultraviolet"
)

func TestHeaderPaintsNativeCellsAndCyclesLayouts(t *testing.T) {
	for demo := range demoCount {
		t.Run(demoName(demo), func(t *testing.T) {
			view := lipgloss.NewCanvas(60, headerRows)
			ctx := frame.DrawContext[UIData]{
				View: view,
				Term: frame.Snapshot{Child: frame.ChildSnapshot{PID: 4321}},
				Data: UIData{Demo: demo},
			}
			drawHeader(ctx)
			text := view.Render()
			if !strings.Contains(text, demoName(demo)) || !strings.Contains(text, "4321") {
				t.Fatalf("header lost live data: %q", text)
			}
			cell := view.CellAt(0, 0)
			if cell == nil || cell.Style.Bg == nil {
				t.Fatalf("header is not native styled cells: %#v", cell)
			}
			if demo == 0 && !sameColor(cell.Style.Bg, lipgloss.Color("#B91C1C")) {
				t.Fatalf("first demo lost red background: %#v", cell.Style.Bg)
			}
		})
	}
}

func sameColor(a, b color.Color) bool {
	ar, ag, ab, aa := a.RGBA()
	br, bg, bb, ba := b.RGBA()
	return ar == br && ag == bg && ab == bb && aa == ba
}

func TestHeaderBackgroundIsIndependentOfLayout(t *testing.T) {
	for demo := range demoCount {
		for background, expected := range []string{"#B91C1C", "#172554", "#115E59"} {
			view := lipgloss.NewCanvas(60, headerRows)
			ctx := frame.DrawContext[UIData]{View: view, Data: UIData{Demo: demo, Background: background}}
			drawHeader(ctx)
			for _, point := range [][2]int{{0, 0}, {59, 2}} {
				cell := view.CellAt(point[0], point[1])
				if cell == nil || cell.Style.Bg == nil || !sameColor(cell.Style.Bg, lipgloss.Color(expected)) {
					t.Fatalf("layout %d background %d did not fill native cell %v: %#v", demo, background, point, cell)
				}
			}
		}
	}
}

func TestFooterUsesChildMetadataAndChangesWithTheDemo(t *testing.T) {
	for demo, bg := range []string{"#0F172A", "#172554", "#115E59"} {
		t.Run(demoName(demo), func(t *testing.T) {
			view := lipgloss.NewCanvas(100, footerRows)
			ctx := frame.DrawContext[UIData]{
				View: view,
				Term: frame.Snapshot{
					Child:    frame.ChildSnapshot{Executable: "/usr/bin/nvim"},
					Terminal: frame.TerminalSnapshot{Title: "grapheme 文 workspace"},
					Viewport: frame.Size{Cols: 80, Rows: 24},
				},
				Data: UIData{Demo: demo},
			}
			drawFooter(ctx)
			for _, text := range []string{"nvim", "80×24", "文", "Ctrl+1", "Ctrl+2", "Ctrl+3", "Ctrl+Q", "border off"} {
				if !strings.Contains(view.Render(), text) {
					t.Errorf("footer omitted %q: %q", text, view.Render())
				}
			}
			cell := view.CellAt(0, 0)
			if cell == nil || cell.Style.Bg == nil || !sameColor(cell.Style.Bg, lipgloss.Color(bg)) {
				t.Fatalf("footer did not adopt demo %d background: %#v", demo, cell)
			}
		})
	}
}

func TestPaintingTinyAndAgentCanvases(t *testing.T) {
	for _, size := range []frame.Size{{}, {Cols: 1, Rows: 1}, {Cols: 8, Rows: 2}} {
		view := lipgloss.NewCanvas(size.Cols, size.Rows)
		ctx := frame.DrawContext[UIData]{View: view, Data: UIData{Agent: true}}
		drawHeader(ctx)
		drawFooter(ctx)
		if strings.Contains(view.Render(), "\x1b[") {
			t.Fatalf("agent chrome added styling: %q", view.Render())
		}
	}
}

func TestDemoKeysUseNativeEventsWithoutCapturingOtherInput(t *testing.T) {
	tests := []struct {
		name string
		key  uv.KeyEvent
		want demoAction
	}{
		{"next", uv.KeyPressEvent{Code: '1', Mod: uv.ModCtrl}, demoNext},
		{"background", uv.KeyPressEvent{Code: '2', Mod: uv.ModCtrl}, demoBackground},
		{"border", uv.KeyPressEvent{Code: '3', Mod: uv.ModCtrl}, demoBorder},
		{"quit", uv.KeyPressEvent{Code: 'q', Mod: uv.ModCtrl}, demoQuit},
		{"ordinary key", uv.KeyPressEvent{Code: 'q', Text: "q"}, demoPass},
		{"plain digit", uv.KeyPressEvent{Code: '1', Text: "1"}, demoPass},
		{"extra modifier", uv.KeyPressEvent{Code: '1', Mod: uv.ModCtrl | uv.ModAlt}, demoPass},
		{"old next function", uv.KeyPressEvent{Code: uv.KeyF6}, demoPass},
		{"old previous function", uv.KeyPressEvent{Code: uv.KeyF5}, demoPass},
		{"selected release", uv.KeyReleaseEvent{Code: '1', Mod: uv.ModCtrl}, demoRelease},
		{"selected release pointer", &uv.KeyReleaseEvent{Code: '1', Mod: uv.ModCtrl}, demoRelease},
		{"background release", uv.KeyReleaseEvent{Code: '2', Mod: uv.ModCtrl}, demoRelease},
		{"border release", uv.KeyReleaseEvent{Code: '3', Mod: uv.ModCtrl}, demoRelease},
		{"old function release", uv.KeyReleaseEvent{Code: uv.KeyF6}, demoPass},
		{"ambiguous NUL", uv.KeyPressEvent{Code: 0, Mod: uv.ModCtrl}, demoPass},
		{"ambiguous ESC", uv.KeyPressEvent{Code: uv.KeyEscape}, demoPass},
		{"ordinary release", uv.KeyReleaseEvent{Code: 'a'}, demoPass},
		{"non-key", nil, demoPass},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte("original bytes")
			if got := actionFor(frame.Input{Raw: raw, Key: tt.key}); got != tt.want {
				t.Fatalf("action=%v, want %v", got, tt.want)
			}
			if string(raw) != "original bytes" {
				t.Fatal("capture altered transport bytes")
			}
		})
	}
}

func TestMetadataIsPaintedAsTextRatherThanTerminalControls(t *testing.T) {
	input := "\x1b[31mproject 文\x1b[0m\nnext\tstep"
	if got := metadataText(input); got != "project 文 next step" {
		t.Fatalf("metadata retained terminal controls: %q", got)
	}
}
