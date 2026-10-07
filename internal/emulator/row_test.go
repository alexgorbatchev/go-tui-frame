package emulator

import (
	"encoding/json"
	"fmt"
	"image/color"
	"slices"
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	ghostty "go.mitchellh.com/libghostty"
)

func TestCellLayoutRejectsUnsupportedManifests(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*packedDescriptor)
	}{
		{"storage", func(cell *packedDescriptor) { cell.Underlying = "u32" }},
		{"missing field", func(cell *packedDescriptor) { delete(cell.Bits, "hyperlink") }},
		{"field type", func(cell *packedDescriptor) {
			field := cell.Bits["wide"]
			field.Type = "bool"
			cell.Bits["wide"] = field
		}},
		{"field width", func(cell *packedDescriptor) {
			field := cell.Bits["style_id"]
			field.LSB, field.Width = 0, packedCellBits
			cell.Bits["style_id"] = field
		}},
		{"zero width", func(cell *packedDescriptor) {
			field := cell.Bits["wide"]
			field.Width = 0
			cell.Bits["wide"] = field
		}},
		{"overflow", func(cell *packedDescriptor) {
			field := cell.Bits["wide"]
			field.LSB = packedCellBits
			cell.Bits["wide"] = field
		}},
		{"content tag", func(cell *packedDescriptor) {
			field := cell.Bits["content"]
			field.Tag = "wide"
			cell.Bits["content"] = field
		}},
		{"missing grapheme arm", func(cell *packedDescriptor) {
			delete(cell.Bits["content"].Arms, "CODEPOINT_GRAPHEME")
		}},
		{"missing background arm", func(cell *packedDescriptor) {
			delete(cell.Bits["content"].Arms, "BG_COLOR_RGB")
		}},
		{"background channel type", func(cell *packedDescriptor) {
			bits := cell.Bits["content"].Arms["BG_COLOR_RGB"].Bits
			field := bits["r"]
			field.Type = "u16"
			bits["r"] = field
		}},
		{"nested overflow", func(cell *packedDescriptor) {
			content := cell.Bits["content"]
			bits := content.Arms["CODEPOINT"].Bits
			field := bits["codepoint"]
			field.LSB = content.Width
			bits["codepoint"] = field
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var manifest struct {
				Schema uint
				Types  map[string]packedDescriptor
			}
			if err := json.Unmarshal([]byte(ghostty.TypeJSON()), &manifest); err != nil {
				t.Fatal(err)
			}
			cell := manifest.Types["GhosttyCell"]
			tt.change(&cell)
			manifest.Types["GhosttyCell"] = cell
			data, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parseCellLayout(string(data)); err == nil {
				t.Fatal("unsupported native cell layout was accepted")
			}
		})
	}
	for _, data := range []string{"{", "{}", `{"schema":2}`} {
		if _, err := parseCellLayout(data); err == nil {
			t.Errorf("unsupported manifest %q was accepted", data)
		}
	}
}

// unchangedMode resets cursor-key mode, which a new terminal already has
// reset. Writing it changes no native value and damages no row.
const unchangedMode = "\x1b[?1l"

func TestRowCaptureAllocationsDoNotScaleWithWidth(t *testing.T) {
	for _, tt := range []struct {
		name string
		// writes cycle per iteration. Erased rows alternate two backgrounds
		// so every iteration damages the row with a different color.
		writes func(cols int) []string
		limit  float64
	}{
		{"plain", func(int) []string { return []string{"\x1b[Hupdated"} }, 12},
		{"styled", func(cols int) []string {
			return []string{"\x1b[H\x1b[38;5;33m" + strings.Repeat("x", cols/2) + "\x1b[38;2;11;22;33m" + strings.Repeat("x", cols-cols/2) + "\x1b[0m"}
		}, 24},
		{"erased palette background", func(int) []string {
			return []string{"\x1b[H\x1b[48;5;33m\x1b[2K\x1b[0m", "\x1b[H\x1b[48;5;34m\x1b[2K\x1b[0m"}
		}, 12},
		{"erased RGB background", func(int) []string {
			return []string{"\x1b[H\x1b[48;2;10;20;30m\x1b[2K\x1b[0m", "\x1b[H\x1b[48;2;40;50;60m\x1b[2K\x1b[0m"}
		}, 12},
	} {
		for _, cols := range []int{20, 120, 240} {
			t.Run(fmt.Sprintf("%s/%d", tt.name, cols), func(t *testing.T) {
				em := newTerminal(t, cols, 3)
				var state State
				if err := em.UpdateState(&state); err != nil {
					t.Fatal(err)
				}
				em.ClearDamage()
				writes := tt.writes(cols)
				capture := func() {
					if err := em.UpdateState(&state); err != nil {
						t.Fatal(err)
					}
					em.ClearDamage()
				}
				// Every write discards the input sample, so the baseline writes
				// a mode the terminal already has, which damages no row.
				undamaged := testing.AllocsPerRun(20, func() {
					writeTerminal(t, em, unchangedMode)
					if err := em.UpdateState(&state); err != nil {
						t.Fatal(err)
					}
					if slices.Contains(em.DirtyRows(), true) {
						t.Fatalf("%q damaged a row", unchangedMode)
					}
				})
				i := 0
				row := testing.AllocsPerRun(20, func() {
					if _, err := em.Write([]byte(writes[i%len(writes)])); err != nil {
						t.Fatal(err)
					}
					i++
					capture()
				})
				t.Logf("undamaged %.0f, damaged row %.0f, additional %.0f allocations", undamaged, row, row-undamaged)
				// Row iterator queries have a fixed cost; cells must not add
				// allocations as the viewport becomes wider.
				if extra := row - undamaged; extra > tt.limit {
					t.Errorf("%d-column row capture added %.0f allocations", cols, extra)
				}
			})
		}
	}
}

// styleReadAllocations is what RenderStateRowCells.Style allocates: the C
// style value and the returned Style. Every dirty row reads each style it
// uses once, because native style IDs are page-local.
const styleReadAllocations = 2

func TestRowCaptureDoesNotReconvertRepeatedStyles(t *testing.T) {
	const cols = 40
	for _, tt := range []struct {
		name string
		// sgrs style consecutive runs of each damaged row; an empty entry
		// writes default-style text.
		sgrs []string
	}{
		{"palette", []string{"38;5;33"}},
		{"RGB", []string{"38;2;11;22;33"}},
		{"styled and plain", []string{"1;38;5;33", "", "4;38;2;11;22;33"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			plain := rowCaptureSlope(t, cols, []string{""})
			styled := rowCaptureSlope(t, cols, tt.sgrs)
			distinct := 0
			for _, sgr := range tt.sgrs {
				if sgr != "" {
					distinct++
				}
			}
			limit := plain + float64(distinct*styleReadAllocations)
			t.Logf("plain row %.1f, styled row %.1f, limit %.1f allocations", plain, styled, limit)
			// Each further row reuses the conversions of the row above, so it
			// costs a plain row plus the native read of each of its styles.
			if styled > limit {
				t.Errorf("each further row with the same styles added %.1f allocations; want at most %.1f", styled, limit)
			}
		})
	}
}

// A row with more styles than rowStyleScanLimit looks its styles up by
// index. Cycling the styles makes later cells return to earlier ones, so each
// lookup must find the style of its own ID, on the first capture and after
// the row is captured again.
func TestManyStyledRowKeepsEachCellStyle(t *testing.T) {
	const cols, styles = 60, 2*rowStyleScanLimit + 3
	em := newTerminal(t, cols, 2)
	want := func(x int) color.RGBA { return color.RGBA{R: uint8(x % styles), G: 9, B: 7, A: 255} }
	var state State
	for _, text := range []string{"a", "b"} {
		var b strings.Builder
		b.WriteString("\x1b[H")
		for x := range cols {
			c := want(x)
			fmt.Fprintf(&b, "\x1b[38;2;%d;%d;%dm%s", c.R, c.G, c.B, text)
		}
		writeTerminal(t, em, b.String()+"\x1b[0m")
		if err := em.UpdateState(&state); err != nil {
			t.Fatal(err)
		}
		for x, cell := range state.Cells[:cols] {
			if cell.Style.Fg != want(x) || cell.Content != text {
				t.Fatalf("capture of %q: cell %d = %q with foreground %#v; want %q with %#v", text, x, cell.Content, cell.Style.Fg, text, want(x))
			}
		}
	}
}

// rowCaptureSlope reports the allocations each further damaged row adds to a
// write and an internal capture, measured from one to four rows that each
// repeat the runs sgrs style.
func rowCaptureSlope(t *testing.T, cols int, sgrs []string) float64 {
	t.Helper()
	measure := func(rows int) float64 {
		em := newTerminal(t, cols, 6)
		var state State
		frames := [2]string{}
		for i, text := range []byte{'a', 'b'} {
			var b strings.Builder
			for y := range rows {
				fmt.Fprintf(&b, "\x1b[%d;1H", y+1)
				for _, sgr := range sgrs {
					fmt.Fprintf(&b, "\x1b[%sm%s\x1b[0m", sgr, strings.Repeat(string(text), cols/len(sgrs)))
				}
			}
			frames[i] = b.String()
		}
		capture := func(frame string) {
			if _, err := em.Write([]byte(frame)); err != nil {
				t.Fatal(err)
			}
			if err := em.UpdateState(&state); err != nil {
				t.Fatal(err)
			}
			if !em.DirtyRows()[rows-1] {
				t.Fatalf("%q left row %d clean", frame, rows-1)
			}
			em.ClearDamage()
		}
		// Warm the converted colors and the visual storage.
		capture(frames[0])
		capture(frames[1])
		i := 0
		return testing.AllocsPerRun(20, func() {
			capture(frames[i%2])
			i++
		})
	}
	one, four := measure(1), measure(4)
	return (four - one) / 3
}

func TestCaptureMatchesNativeCellData(t *testing.T) {
	for _, tt := range []struct {
		name, text string
		// linkedBefore is output captured first. It must link cells that
		// text then overwrites, so the second capture reuses linked storage.
		linkedBefore string
	}{
		{"ASCII", "ASCII \x1b[2;4Hoffset", ""},
		{"graphemes and wide spacers", "e\u0301 界 👩🏽‍💻\r\n\x1b[2;8H界", ""},
		{"full styles", "\x1b[1;3;53;4:3;38;2;10;20;30;58;2;40;50;60mstyled\x1b[0m\r\nplain", ""},
		{"background-only cells", "\x1b[48;2;10;20;30m\x1b[2K\x1b[0m\r\n\x1b[44m\x1b[2K\x1b[0m", ""},
		{"hyperlinks", "\x1b]8;;https://example.test/report\x1b\\link界\x1b]8;;\x1b\\ plain", ""},
		{"plain text over hyperlinks", "\x1b[Hplain 界", "\x1b]8;;https://example.test/report\x1b\\linked界 row\x1b]8;;\x1b\\"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			em := newTerminal(t, 8, 5)
			if tt.linkedBefore != "" {
				writeTerminal(t, em, tt.linkedBefore)
				if !slices.ContainsFunc(terminalState(t, em).Cells, func(c uv.Cell) bool { return c.Link.URL != "" }) {
					t.Fatal("linkedBefore output captured no hyperlinked cell")
				}
			}
			writeTerminal(t, em, tt.text)
			state := terminalState(t, em)
			if err := em.render.RowIterator(em.rows); err != nil {
				t.Fatal(err)
			}
			for y := 0; em.rows.Next(); y++ {
				if err := em.rows.Cells(em.cells); err != nil {
					t.Fatal(err)
				}
				for x := range state.Size.Cols {
					if err := em.cells.Select(uint16(x)); err != nil {
						t.Fatal(err)
					}
					raw, err := em.cells.Raw()
					if err != nil {
						t.Fatal(err)
					}
					wide, err := raw.Wide()
					if err != nil {
						t.Fatal(err)
					}
					codepoint, err := raw.Codepoint()
					if err != nil {
						t.Fatal(err)
					}
					tag, err := raw.ContentTag()
					if err != nil {
						t.Fatal(err)
					}
					styled, err := raw.HasStyling()
					if err != nil {
						t.Fatal(err)
					}
					styleID, err := raw.StyleID()
					if err != nil {
						t.Fatal(err)
					}
					style, err := em.cells.Style()
					if err != nil {
						t.Fatal(err)
					}
					graphemes, err := em.cells.AppendGraphemes(nil)
					if err != nil {
						t.Fatal(err)
					}
					content := string(graphemes)
					if wide == ghostty.CellWideSpacerTail {
						content = ""
					} else if content == "" || wide == ghostty.CellWideSpacerHead {
						content = " "
					}
					i := y*state.Size.Cols + x
					cell, native := state.Cells[i], state.NativeCells[i]
					bg, err := em.cells.BgColor()
					if err != nil {
						t.Fatal(err)
					}
					if bg != nil && cell.Style.Bg != rgbColor(*bg) {
						t.Fatalf("cell %d,%d background = %#v; native = %#v", x, y, cell.Style.Bg, *bg)
					}
					if native.Raw != *raw || native.Wide != wide || state.NativeStyles[native.StyleIndex] != *style || cell.Content != content {
						t.Fatalf("cell %d,%d differs from native text, width, raw value or full style", x, y)
					}
					linked, err := raw.HasHyperlink()
					if err != nil {
						t.Fatal(err)
					}
					decoded := em.layout.decode(raw.PackedValue())
					if decoded.codepoint != codepoint || decoded.tag != tag || decoded.wide != wide || decoded.styleID != styleID || (decoded.styleID != 0) != styled || decoded.linked != linked {
						t.Fatalf("cell %d,%d manifest decode differs from Ghostty's getters", x, y)
					}
					if linked != (cell.Link.URL != "") || !linked && cell.Link != (uv.Link{}) {
						t.Fatalf("cell %d,%d lost or invented a hyperlink: %#v", x, y, cell.Link)
					}
				}
			}
		})
	}
}
