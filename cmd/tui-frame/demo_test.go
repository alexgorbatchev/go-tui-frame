package main

import (
	"fmt"
	"image/color"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	frame "github.com/alexgorbatchev/go-tui-frame"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

func TestHeaderPaintsNativeCellsAndCyclesLayouts(t *testing.T) {
	for demo := range demoCount {
		t.Run(demoName(demo), func(t *testing.T) {
			view := newDemoTestScreen(60, headerRows)
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
			view := newDemoTestScreen(60, headerRows)
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
	const hints = "Ctrl+1 layout | Ctrl+2 colour | Ctrl+3 border | Ctrl+Q quit"
	sizes := []struct {
		name         string
		width        int
		title        string
		cutsMetadata bool
		cutsKeyHints bool
	}{
		{name: "short title", width: 100, title: "grapheme 文 workspace"},
		{name: "long path title", width: 100, cutsMetadata: true,
			title: "nvim /home/user/projects/go-tui-frame/internal/emulator/state.go (~/projects/go-tui-frame)"},
		{name: "long CJK and emoji title", width: 100, cutsMetadata: true, title: strings.Repeat("文👩‍💻", 30)},
		{name: "narrow footer", width: 40, cutsMetadata: true, cutsKeyHints: true, title: "grapheme 文 workspace"},
	}
	layouts := []struct {
		name          string
		data          UIData
		bg            string
		borderColumns int
	}{
		{name: demoName(0), data: UIData{Demo: 0}, bg: "#0F172A"},
		{name: demoName(1), data: UIData{Demo: 1}, bg: "#172554"},
		{name: demoName(2), data: UIData{Demo: 2}, bg: "#115E59", borderColumns: 1},
		{name: "agent", data: UIData{Agent: true}},
	}
	for _, size := range sizes {
		for _, layout := range layouts {
			for _, border := range []string{"off", "on"} {
				t.Run(size.name+"/"+layout.name+"/border "+border, func(t *testing.T) {
					data := layout.data
					data.Border = border == "on"
					view := newDemoTestScreen(size.width, footerRows)
					drawFooter(frame.DrawContext[UIData]{
						View: view,
						Term: frame.Snapshot{
							Child:    frame.ChildSnapshot{Executable: "/usr/bin/nvim"},
							Terminal: frame.TerminalSnapshot{Title: size.title},
							Viewport: frame.Size{Cols: 80, Rows: 24},
						},
						Data: data,
					})
					width := size.width - layout.borderColumns
					metadata := fmt.Sprintf("nvim | 80×24 | border %s | %s", border, size.title)
					assertFooterRow(t, footerRowText(t, view, 0, layout.borderColumns), metadata, width, size.cutsMetadata)
					assertFooterRow(t, footerRowText(t, view, 1, layout.borderColumns), hints, width, size.cutsKeyHints)
					if data.Agent {
						if strings.Contains(view.Render(), "\x1b[") {
							t.Fatalf("agent footer added styling: %q", view.Render())
						}
						return
					}
					cell := view.CellAt(0, 0)
					if cell == nil || cell.Style.Bg == nil || !sameColor(cell.Style.Bg, lipgloss.Color(layout.bg)) {
						t.Fatalf("footer did not adopt %s background: %#v", layout.name, cell)
					}
				})
			}
		}
	}
}

// footerRowText returns the plain text of footer row y after the layout's
// left border columns, without trailing padding.
func footerRowText(t *testing.T, view uv.ScreenBuffer, y, borderColumns int) string {
	t.Helper()
	row := strings.TrimRight(view.Line(y).String(), " ")
	if borderColumns == 0 {
		return row
	}
	text, ok := strings.CutPrefix(row, strings.Repeat("│", borderColumns))
	if !ok {
		t.Fatalf("footer row %d lost its left border: %q", y, row)
	}
	return text
}

// assertFooterRow checks that a footer row shows text whole when it fits in
// width cells, and otherwise shows the graphemes that fit before an ellipsis.
func assertFooterRow(t *testing.T, row, text string, width int, cut bool) {
	t.Helper()
	if !cut {
		if row != text {
			t.Errorf("footer row = %q, want %q", row, text)
		}
		return
	}
	shown, ok := strings.CutSuffix(row, "…")
	// The widest grapheme takes two cells, so a cut row fills its width or
	// stops one cell short of it.
	if !ok || !strings.HasPrefix(text, shown) || !isGraphemeBoundary(text, len(shown)) || ansi.StringWidth(row) < width-1 {
		t.Errorf("footer row = %q (%d cells), want %q cut to %d cells with an ellipsis", row, ansi.StringWidth(row), text, width)
	}
}

func isGraphemeBoundary(text string, offset int) bool {
	for at := 0; at < offset; {
		cluster, _ := ansi.FirstGraphemeCluster(text[at:], ansi.GraphemeWidth)
		at += len(cluster)
		if at == offset {
			return true
		}
	}
	return offset == 0
}

func TestPaintingTinyAndAgentCanvases(t *testing.T) {
	for _, size := range []frame.Size{{}, {Cols: 1, Rows: 1}, {Cols: 8, Rows: 2}} {
		view := newDemoTestScreen(size.Cols, size.Rows)
		ctx := frame.DrawContext[UIData]{View: view, Data: UIData{Agent: true}}
		drawHeader(ctx)
		drawFooter(ctx)
		if strings.Contains(view.Render(), "\x1b[") {
			t.Fatalf("agent chrome added styling: %q", view.Render())
		}
	}
}

func TestDemoKeysUseNativeEventsWithoutCapturingOtherInput(t *testing.T) {
	type keyCase struct {
		name string
		key  uv.KeyEvent
		want demoAction
	}
	tests := []keyCase{
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
		{"extra modifier with caps lock", uv.KeyPressEvent{Code: '1', Mod: uv.ModCtrl | uv.ModAlt | uv.ModCapsLock}, demoPass},
	}
	// A Kitty terminal sets an enabled lock's bit on every control it
	// reports, so a lock must not stop a control from matching.
	for _, lock := range []struct {
		name string
		mod  uv.KeyMod
	}{{"caps lock", uv.ModCapsLock}, {"num lock", uv.ModNumLock}} {
		for _, control := range []struct {
			name string
			code rune
			want demoAction
		}{{"next", '1', demoNext}, {"background", '2', demoBackground}, {"border", '3', demoBorder}, {"quit", 'q', demoQuit}} {
			mod := uv.ModCtrl | lock.mod
			tests = append(tests,
				keyCase{control.name + " with " + lock.name, uv.KeyPressEvent{Code: control.code, Mod: mod}, control.want},
				keyCase{control.name + " release with " + lock.name, uv.KeyReleaseEvent{Code: control.code, Mod: mod}, demoRelease},
			)
		}
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

func newDemoTestScreen(width, height int) uv.ScreenBuffer {
	view := uv.NewScreenBuffer(width, height)
	view.Method = ansi.GraphemeWidth
	return view
}

func TestDemoAcceptsOptionalLipGlossCanvas(t *testing.T) {
	view := lipgloss.NewCanvas(60, headerRows)
	drawHeader(frame.DrawContext[UIData]{View: view, Data: UIData{Demo: 1}})
	if text := view.Render(); !strings.Contains(text, demoName(1)) || !strings.Contains(text, "Ctrl+1 layout") {
		t.Fatalf("Lip Gloss drawable lost content: %q", text)
	}
}
