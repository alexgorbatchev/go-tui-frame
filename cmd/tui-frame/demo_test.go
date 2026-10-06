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
	const hints = "Ctrl+B 1 layout | Ctrl+B 2 colour | Ctrl+B 3 border | Ctrl+Q quit"
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
		boldKeyHints  bool
	}{
		{name: demoName(0), data: UIData{Demo: 0}, bg: "#0F172A"},
		{name: demoName(1), data: UIData{Demo: 1}, bg: "#172554", boldKeyHints: true},
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
					hintRow := footerRowText(t, view, 1, layout.borderColumns)
					assertFooterRow(t, footerRowText(t, view, 0, layout.borderColumns), metadata, width, size.cutsMetadata)
					assertFooterRow(t, hintRow, hints, width, size.cutsKeyHints)
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
					assertBoldCells(t, view, 0, 0, size.width, false)
					hintEnd := layout.borderColumns + ansi.StringWidth(hintRow)
					assertBoldCells(t, view, 1, layout.borderColumns, hintEnd, layout.boldKeyHints)
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
// width cells, and otherwise shows the longest run of leading graphemes that
// fits before a one-cell ellipsis.
func assertFooterRow(t *testing.T, row, text string, width int, cut bool) {
	t.Helper()
	if !cut {
		if row != text {
			t.Errorf("footer row = %q, want %q", row, text)
		}
		return
	}
	budget := width - 1 // The ellipsis takes the last cell.
	shown, ok := strings.CutSuffix(row, "…")
	// A grapheme split by the cut joins its remainder here, adds no width and
	// fails the longest-prefix condition as well.
	next, _ := ansi.FirstGraphemeCluster(strings.TrimPrefix(text, shown), ansi.GraphemeWidth)
	if !ok || !strings.HasPrefix(text, shown) || ansi.StringWidth(shown) > budget || ansi.StringWidth(shown+next) <= budget {
		t.Errorf("footer row = %q (%d cells), want the longest leading graphemes of %q within %d cells, then an ellipsis",
			row, ansi.StringWidth(row), text, budget)
	}
}

// assertBoldCells checks that every cell of row y from column from up to
// column to is bold exactly when want is set.
func assertBoldCells(t *testing.T, view uv.ScreenBuffer, y, from, to int, want bool) {
	t.Helper()
	for x := from; x < to; x++ {
		cell := view.CellAt(x, y)
		if cell == nil {
			t.Fatalf("footer row %d has no cell at column %d", y, x)
		}
		if bold := cell.Style.Attrs&uv.AttrBold != 0; bold != want {
			t.Errorf("footer cell (%d, %d) %q bold = %v, want %v", x, y, cell.Content, bold, want)
			return
		}
	}
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

// The demo binds a tmux-style prefix: Ctrl+B, then 1, 2 or 3 runs an action,
// a second Ctrl+B passes to the child, and any other key ends the prefix and
// is discarded. Ctrl+Q quits without the prefix. Releases, repeats and lone
// modifier or lock keys, which Kitty terminals report, leave the prefix as it
// is; the frame gives a reported release its press's disposition.
func TestDemoPrefixKeys(t *testing.T) {
	type step struct {
		key         uv.KeyEvent
		action      demoAction
		disposition frame.Disposition
	}
	ctrlB := uv.KeyPressEvent{Code: 'b', Mod: uv.ModCtrl}
	digit := func(r rune) uv.KeyPressEvent { return uv.KeyPressEvent{Code: r, Text: string(r)} }
	prefixed := func(key uv.KeyEvent, action demoAction, disposition frame.Disposition) []step {
		return []step{{ctrlB, demoNone, frame.Consume}, {key, action, disposition}}
	}
	tests := []struct {
		name  string
		steps []step
	}{
		{"layout", prefixed(digit('1'), demoNext, frame.Consume)},
		{"colour", prefixed(digit('2'), demoBackground, frame.Consume)},
		{"border", prefixed(digit('3'), demoBorder, frame.Consume)},
		{"quit", []step{{uv.KeyPressEvent{Code: 'q', Mod: uv.ModCtrl}, demoQuit, frame.Consume}}},
		{"second Ctrl+B passes and ends the prefix", append(prefixed(ctrlB, demoNone, frame.Pass), step{digit('1'), demoNone, frame.Pass})},
		{"other key ends the prefix and is discarded", append(prefixed(digit('x'), demoNone, frame.Consume), step{digit('1'), demoNone, frame.Pass})},
		{"Ctrl+Q after the prefix is discarded", append(prefixed(uv.KeyPressEvent{Code: 'q', Mod: uv.ModCtrl}, demoNone, frame.Consume),
			step{uv.KeyPressEvent{Code: 'q', Mod: uv.ModCtrl}, demoQuit, frame.Consume})},
		{"modified digit after the prefix is discarded", prefixed(uv.KeyPressEvent{Code: '1', Mod: uv.ModCtrl}, demoNone, frame.Consume)},
		{"release between the keys", []step{
			{ctrlB, demoNone, frame.Consume}, {uv.KeyReleaseEvent{Code: 'b', Mod: uv.ModCtrl}, demoNone, frame.Pass}, {digit('1'), demoNext, frame.Consume},
		}},
		{"release pointer between the keys", []step{
			{ctrlB, demoNone, frame.Consume}, {&uv.KeyReleaseEvent{Code: 'b', Mod: uv.ModCtrl}, demoNone, frame.Pass}, {digit('2'), demoBackground, frame.Consume},
		}},
		{"repeat between the keys", []step{
			{ctrlB, demoNone, frame.Consume}, {uv.KeyPressEvent{Code: 'b', Mod: uv.ModCtrl, IsRepeat: true}, demoNone, frame.Pass}, {digit('2'), demoBackground, frame.Consume},
		}},
		{"modifier and lock keys between the keys", []step{
			{ctrlB, demoNone, frame.Consume}, {uv.KeyPressEvent{Code: uv.KeyLeftCtrl, Mod: uv.ModCtrl}, demoNone, frame.Pass},
			{uv.KeyPressEvent{Code: uv.KeyCapsLock}, demoNone, frame.Pass}, {digit('3'), demoBorder, frame.Consume},
		}},
		{"pointer press", []step{{&ctrlB, demoNone, frame.Consume}, {digit('1'), demoNext, frame.Consume}}},
		// A Kitty terminal sets an enabled lock's bit on every key it reports.
		{"locks", []step{
			{uv.KeyPressEvent{Code: 'b', Mod: uv.ModCtrl | uv.ModCapsLock}, demoNone, frame.Consume},
			{uv.KeyPressEvent{Code: '1', Text: "1", Mod: uv.ModNumLock}, demoNext, frame.Consume},
			{uv.KeyPressEvent{Code: 'q', Mod: uv.ModCtrl | uv.ModNumLock}, demoQuit, frame.Consume},
		}},
		{"plain digit", []step{{digit('1'), demoNone, frame.Pass}}},
		{"Ctrl+number", []step{{uv.KeyPressEvent{Code: '1', Mod: uv.ModCtrl}, demoNone, frame.Pass}}},
		{"ordinary key", []step{{digit('q'), demoNone, frame.Pass}}},
		{"function key", []step{{uv.KeyPressEvent{Code: uv.KeyF5}, demoNone, frame.Pass}}},
		{"ambiguous NUL", []step{{uv.KeyPressEvent{Code: 0, Mod: uv.ModCtrl}, demoNone, frame.Pass}}},
		{"ordinary release", []step{{uv.KeyReleaseEvent{Code: 'a'}, demoNone, frame.Pass}}},
		{"non-key", []step{{nil, demoNone, frame.Pass}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var keys keyPrefix
			for i, s := range tt.steps {
				raw := []byte("original bytes")
				action, disposition := keys.route(frame.Input{Raw: raw, Key: s.key})
				if action != s.action || disposition != s.disposition {
					t.Fatalf("step %d %#v: action=%v disposition=%v, want %v and %v", i, s.key, action, disposition, s.action, s.disposition)
				}
				if string(raw) != "original bytes" {
					t.Fatal("capture altered transport bytes")
				}
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
	if text := view.Render(); !strings.Contains(text, demoName(1)) || !strings.Contains(text, "Ctrl+B 1 layout") {
		t.Fatalf("Lip Gloss drawable lost content: %q", text)
	}
}
