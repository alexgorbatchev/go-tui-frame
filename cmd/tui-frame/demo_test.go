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
			for _, text := range []string{"nvim", "80×24", "文", "F6", "Ctrl+Q"} {
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
		{"next", uv.KeyPressEvent{Code: uv.KeyF6}, demoNext},
		{"previous", uv.KeyPressEvent{Code: uv.KeyF5}, demoPrevious},
		{"quit", uv.KeyPressEvent{Code: 'q', Mod: uv.ModCtrl}, demoQuit},
		{"ordinary key", uv.KeyPressEvent{Code: 'q', Text: "q"}, demoPass},
		{"modified function", uv.KeyPressEvent{Code: uv.KeyF6, Mod: uv.ModAlt}, demoPass},
		{"selected release", uv.KeyReleaseEvent{Code: uv.KeyF6}, demoRelease},
		{"selected release pointer", &uv.KeyReleaseEvent{Code: uv.KeyF6}, demoRelease},
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
