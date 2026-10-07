package emulator

import (
	"strconv"
	"strings"
	"testing"
)

// syncFrame wraps text in a DEC 2026 synchronized update.
func syncFrame(text string) string { return "\x1b[?2026h" + text + "\x1b[?2026l" }

// fillFrame homes the cursor and fills rows rows of cols cells with ch.
func fillFrame(ch string, cols, rows int) string {
	return "\x1b[H" + strings.Repeat(strings.Repeat(ch, cols)+"\r\n", rows-1) + strings.Repeat(ch, cols)
}

// Frames that complete and are superseded within one write are never shown,
// so no capture may convert them: the converted cells and damage keep the
// last capture until a read of the state, which converts the frame it shows
// once.
func TestSynchronizedFramesAreConvertedOnlyWhenRead(t *testing.T) {
	const cols, rows = 8, 3
	tests := []struct {
		name string
		// output follows a captured frame of "o" cells in one write.
		output string
		held   bool
		want   string
	}{
		{
			name:   "every frame closed",
			output: syncFrame(fillFrame("a", cols, rows)) + syncFrame(fillFrame("b", cols, rows)) + syncFrame(fillFrame("c", cols, rows)),
			want:   "c",
		},
		{
			name:   "last frame held",
			output: syncFrame(fillFrame("a", cols, rows)) + syncFrame(fillFrame("b", cols, rows)) + "\x1b[?2026h" + fillFrame("c", cols, rows),
			held:   true,
			want:   "b",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			em := newTerminal(t, cols, rows)
			writeTerminal(t, em, fillFrame("o", cols, rows))
			terminalState(t, em)
			em.ClearDamage()
			writeTerminal(t, em, tt.output)
			for i, cell := range em.visual.cells {
				if cell.Content != "o" {
					t.Fatalf("write converted an undisplayed frame: cell %d = %q, want the last capture's %q", i, cell.Content, "o")
				}
			}
			for y, dirty := range em.DirtyRows() {
				if dirty {
					t.Fatalf("write converted row %d of an undisplayed frame", y)
				}
			}
			s := terminalState(t, em)
			if s.Held != tt.held {
				t.Fatalf("Held = %v, want %v", s.Held, tt.held)
			}
			for i, cell := range s.Cells {
				if cell.Content != tt.want {
					t.Fatalf("state cell %d = %q, want %q", i, cell.Content, tt.want)
				}
			}
			for y, dirty := range em.DirtyRows() {
				if !dirty {
					t.Fatalf("reading the state left converted row %d clean", y)
				}
			}
		})
	}
}

// A held frame keeps the text and links it had when the hold began, even when
// later bytes of the same write replace them before any read of the state.
func TestHeldFrameKeepsTextAndLinksOverwrittenInTheHold(t *testing.T) {
	const link = "\x1b]8;;https://example.test/held\x1b\\held\x1b]8;;\x1b\\"
	const replacement = "\x1b[H\x1b]8;;https://example.test/next\x1b\\next\x1b]8;;\x1b\\"
	tests := []struct {
		name string
		// before is written and captured before the hold.
		before string
		// frame completes the frame the hold preserves.
		frame string
		// hold begins the update and overwrites the frame.
		hold    string
		wantURL string
	}{
		{name: "plain", frame: "\x1b[Hheld", hold: "\x1b[?2026h\x1b[Hnext"},
		{name: "plain overwritten by a link", frame: "\x1b[Hheld", hold: "\x1b[?2026h" + replacement},
		{name: "link written before the hold", frame: "\x1b[H" + link, hold: "\x1b[?2026h" + replacement, wantURL: "https://example.test/held"},
		{
			// A default-color change invalidates every row, so a conversion
			// reads the clean linked row as well.
			name: "captured link recolored before the hold", before: "\x1b[H" + link,
			frame: "\x1b]10;#123456\a", hold: "\x1b[?2026h" + replacement, wantURL: "https://example.test/held",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			em := newTerminal(t, 8, 2)
			writeTerminal(t, em, tt.before)
			terminalState(t, em)
			writeTerminal(t, em, tt.frame+tt.hold)
			s := terminalState(t, em)
			if !s.Held {
				t.Fatal("synchronized update did not hold the frame")
			}
			var text strings.Builder
			for x, cell := range s.Cells[:4] {
				text.WriteString(cell.Content)
				if cell.Link.URL != tt.wantURL {
					t.Fatalf("held cell %d link = %q, want %q", x, cell.Link.URL, tt.wantURL)
				}
			}
			if text.String() != "held" {
				t.Fatalf("held text = %q, want %q", text.String(), "held")
			}
			if err := em.ReleaseHold(); err != nil {
				t.Fatal(err)
			}
			s = terminalState(t, em)
			if s.Held || s.Cells[0].Content != "n" {
				t.Fatalf("release did not reveal the held update: held %v, cell %#v", s.Held, s.Cells[0])
			}
		})
	}
}

// BenchmarkSynchronizedFrames writes two synchronized frames per write, so
// each write completes a frame no read displays.
func BenchmarkSynchronizedFrames(b *testing.B) {
	const cols, rows = 120, 40
	frame := func(ch string) string {
		var s strings.Builder
		for y := range 30 {
			s.WriteString("\x1b[" + strconv.Itoa(y+1) + ";1H" + strings.Repeat(ch, 100))
		}
		return s.String()
	}
	for _, tt := range []struct{ name, text string }{
		{"unsynchronized", frame("a") + frame("b")},
		{"synchronized", syncFrame(frame("a")) + syncFrame(frame("b"))},
	} {
		b.Run(tt.name, func(b *testing.B) {
			em, err := New(Options{Size: Size{Cols: cols, Rows: rows}})
			if err != nil {
				b.Fatal(err)
			}
			defer em.Close()
			var state State
			if err := em.UpdateState(&state); err != nil {
				b.Fatal(err)
			}
			text := []byte(tt.text)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := em.Write(text); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
