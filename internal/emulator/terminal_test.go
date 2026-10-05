package emulator

import (
	"errors"
	"image/color"
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	ghostty "go.mitchellh.com/libghostty"
)

func newTerminal(t *testing.T, cols, rows int) *Terminal {
	t.Helper()
	em, err := New(Options{Size: Size{Cols: cols, Rows: rows}, TerminfoName: "xterm-256color"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(em.Close)
	return em
}

func writeTerminal(t *testing.T, em *Terminal, text string) {
	t.Helper()
	n, err := em.Write([]byte(text))
	if err != nil || n != len(text) {
		t.Fatalf("Write(%q) = %d, %v", text, n, err)
	}
}

func terminalState(t *testing.T, em *Terminal) State {
	t.Helper()
	s, err := em.State()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFragmentedOutputAndOwnedCells(t *testing.T) {
	em := newTerminal(t, 12, 3)
	for _, fragment := range []string{"\x1b[2;", "3H", "\x1b[1;3;4:3;5;8;9;53;38;2;10;20;30;58;2;40;50;60m", "e", "\xcc", "\x81", "\xe7", "\x95\x8c"} {
		writeTerminal(t, em, fragment)
	}
	s := terminalState(t, em)
	idx := s.Size.Cols + 2
	cell := s.Cells[idx]
	if cell.Content != "é" || cell.Width != 1 || s.Cells[idx+1].Content != "界" || s.Cells[idx+1].Width != 2 || s.Cells[idx+2].Width != 0 {
		t.Fatalf("graphemes/widths = %#v", s.Cells[idx:idx+3])
	}
	wantAttrs := uint8(uv.AttrBold | uv.AttrItalic | uv.AttrBlink | uv.AttrConceal | uv.AttrStrikethrough)
	if cell.Style.Attrs != wantAttrs || cell.Style.Underline != uv.UnderlineCurly {
		t.Fatalf("style = %#v", cell.Style)
	}
	if cell.Style.Fg != (color.RGBA{R: 10, G: 20, B: 30, A: 255}) || cell.Style.UnderlineColor != (color.RGBA{R: 40, G: 50, B: 60, A: 255}) {
		t.Fatalf("colors = %#v", cell.Style)
	}
	if !s.NativeCells[idx].Style.Overline() {
		t.Fatal("native overline state was lost")
	}
	s.Cells[idx].Content = "changed by observer"
	writeTerminal(t, em, "\x1b[1;1HX")
	next := terminalState(t, em)
	if next.Cells[idx].Content != "é" || next.Cells[0].Content != "X" {
		t.Fatalf("owned snapshots = %#v, %#v", s.Cells[idx], next.Cells[idx])
	}
	if !s.NativeCells[idx].Style.Overline() {
		t.Fatal("copied native style changed after write")
	}
}

func TestQueryEffectsRemainOrderedAndOwned(t *testing.T) {
	em := newTerminal(t, 12, 3)
	writeTerminal(t, em, "\x1b[2;4H\x1b[6n\a\x1b[?2004h\x1b[?2004$p\x1b[>1u\x1b[?u\x1b[18t")
	effects := em.Effects()
	var got []string
	for _, effect := range effects {
		if effect.Kind == Reply {
			got = append(got, string(effect.Bytes))
		} else if effect.Kind == Bell {
			got = append(got, "bell")
		}
	}
	want := []string{"\x1b[2;4R", "bell", "\x1b[?2004;1$y", "\x1b[?1u", "\x1b[8;3;12t"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("effects = %q, want %q", got, want)
	}
	if len(em.Effects()) != 0 {
		t.Fatal("effects drain retained consumed records")
	}
	writeTerminal(t, em, "\x1b[1;1H\x1b[6n")
	if string(effects[0].Bytes) != want[0] {
		t.Fatal("borrowed native reply escaped its lifetime")
	}
}

func TestModesAlternateScreenAndReset(t *testing.T) {
	em := newTerminal(t, 12, 3)
	writeTerminal(t, em, "normal\x1b]2;child title\x1b\\\x1b]7;file://host/work\x1b\\\x1b[?2004;1004;1002;1006h\x1b[>5u")
	s := terminalState(t, em)
	for _, mode := range []ghostty.Mode{ghostty.ModeBracketedPaste, ghostty.ModeFocusEvent, ghostty.ModeButtonMouse, ghostty.ModeSGRMouse} {
		if !s.Modes[mode] {
			t.Fatalf("mode %d not enabled", mode.Value())
		}
	}
	if s.Title != "child title" || s.Directory != "file://host/work" || s.KittyKeyboardFlags != 5 {
		t.Fatalf("terminal observations = %#v", s)
	}
	// Input routing observes the active screen without capturing the viewport.
	var input State
	writeTerminal(t, em, "\x1b[?1049h\x1b[HALT")
	if err := em.InputState(&input); err != nil || !input.Alternate {
		t.Fatalf("input alternate screen = %v, %v", input.Alternate, err)
	}
	s = terminalState(t, em)
	if !s.Alternate || s.Cells[0].Content != "A" {
		t.Fatalf("alternate screen = %v, %#v", s.Alternate, s.Cells[0])
	}
	writeTerminal(t, em, "\x1b[?1049l")
	if err := em.InputState(&input); err != nil || input.Alternate {
		t.Fatalf("input alternate screen after exit = %v, %v", input.Alternate, err)
	}
	s = terminalState(t, em)
	if s.Alternate || s.Cells[0].Content != "n" {
		t.Fatal("primary screen was not restored")
	}
	writeTerminal(t, em, "\x1bc")
	s = terminalState(t, em)
	if s.Title != "" || s.Directory != "" || s.Modes[ghostty.ModeBracketedPaste] || s.KittyKeyboardFlags != 0 {
		t.Fatal("RIS reset left stale observations")
	}
}

func TestRenderHoldCapturesFrameBeforeRemainingBytes(t *testing.T) {
	em := newTerminal(t, 8, 2)
	writeTerminal(t, em, "old")
	writeTerminal(t, em, "\x1b[?2026h\x1b[Hnew")
	s := terminalState(t, em)
	if !s.Held || s.Cells[0].Content != "o" {
		t.Fatalf("held frame = %v, %#v", s.Held, s.Cells[0])
	}
	if err := em.ReleaseHold(); err != nil {
		t.Fatal(err)
	}
	s = terminalState(t, em)
	if s.Held || s.Cells[0].Content != "n" {
		t.Fatal("release did not reveal completed output")
	}
}

func TestResizeQueriesScrollbackAndHyperlinks(t *testing.T) {
	em := newTerminal(t, 8, 2)
	writeTerminal(t, em, "1\r\n2\r\n3\r\n4")
	s := terminalState(t, em)
	if s.ScrollbackRows < 2 || s.TotalRows != s.ScrollbackRows+2 || s.Scrollbar.Len != 2 {
		t.Fatalf("scrollback = %d total = %d scrollbar = %#v", s.ScrollbackRows, s.TotalRows, s.Scrollbar)
	}
	if err := em.Resize(Size{Cols: 10, Rows: 4, CellWidthPx: 9, CellHeightPx: 18}); err != nil {
		t.Fatal(err)
	}
	writeTerminal(t, em, "\x1b[H\x1b]8;id=original;https://example.test/path\x1b\\link\x1b]8;;\x1b\\\x1b[14t\x1b[16t\x1b[18t")
	s = terminalState(t, em)
	if s.Size.Cols != 10 || s.Size.Rows != 4 || s.Cells[0].Link.URL != "https://example.test/path" {
		t.Fatalf("resized hyperlink state = %#v, %#v", s.Size, s.Cells[0])
	}
	var replies strings.Builder
	for _, effect := range em.Effects() {
		if effect.Kind == Reply {
			replies.Write(effect.Bytes)
		}
	}
	if got, want := replies.String(), "\x1b[4;72;90t\x1b[6;18;9t\x1b[8;4;10t"; got != want {
		t.Fatalf("size query replies = %q, want %q", got, want)
	}
}

func TestMetadataEffectsAndClipboardPolicy(t *testing.T) {
	em := newTerminal(t, 8, 2)
	writeTerminal(t, em, "\x1b]2;first\x07\x1b]7;file://host/work\x07\x1b]52;c;aGVsbG8=\x1b\\\x1b]9999;unknown\x07")
	effects := em.Effects()
	var title, directory string
	var clipboard *ghostty.ClipboardWrite
	var unknown *ghostty.TerminalUnknownSequence
	for _, effect := range effects {
		switch effect.Kind {
		case TitleChanged:
			title = effect.Text
		case DirectoryChanged:
			directory = effect.Text
		case ClipboardWrite:
			clipboard = effect.ClipboardWrite
		case Unknown:
			unknown = effect.Unknown
		}
	}
	if title != "first" || directory != "file://host/work" || clipboard == nil || len(clipboard.Contents) != 1 || string(clipboard.Contents[0].Data) != "hello" || unknown == nil || string(unknown.OSC.Content) != "9999;unknown" {
		t.Fatalf("effects = %#v", effects)
	}
	writeTerminal(t, em, "\x1b]2;second\x07\x1b]52;c;eA==\x1b\\")
	em.Close()
	if title != "first" || string(clipboard.Contents[0].Data) != "hello" || string(unknown.OSC.Content) != "9999;unknown" {
		t.Fatal("effect memory changed after later writes/close")
	}
}

func TestInvalidGeometryAndClosedOperations(t *testing.T) {
	for _, size := range []Size{{Cols: 0, Rows: 2}, {Cols: -1, Rows: 2}, {Cols: 2, Rows: 65536}} {
		if em, err := New(Options{Size: size}); err == nil {
			em.Close()
			t.Fatalf("accepted geometry %#v", size)
		}
	}
	em := newTerminal(t, 8, 2)
	if err := em.Resize(Size{Cols: 65536, Rows: 2}); err == nil {
		t.Fatal("accepted resize overflow")
	}
	em.Close()
	em.Close()
	if _, err := em.Write([]byte("x")); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed Write = %v", err)
	}
	if _, err := em.State(); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed State = %v", err)
	}
	if err := em.Resize(Size{Cols: 8, Rows: 2}); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed Resize = %v", err)
	}
}
