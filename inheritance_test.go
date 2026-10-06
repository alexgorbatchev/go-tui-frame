package frame

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/color"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexgorbatchev/go-tui-frame/internal/emulator"
	"github.com/alexgorbatchev/go-tui-frame/internal/input"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	ghostty "go.mitchellh.com/libghostty"
	"golang.org/x/sys/unix"
)

func TestSessionInheritsOuterTerminalDefaults(t *testing.T) {
	h := newHarness(t)
	h.mu.Lock()
	_, err := h.em.Write([]byte("\x1b]10;#d0c0b0\x07\x1b]11;#203040\x07\x1b]12;#abcdef\x07\x1b]4;1;#123456;200;#654321\x07\x1b[6 q\x1b[?1h"))
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan Snapshot, 1)
	done := make(chan error, 1)
	app := New(childCommand(t), struct{}{}).Terminal(h.slave, h.slave).Observe(func(e Event) {
		if e.Kind == Started {
			started <- *e.Snapshot
		}
	})
	go func() { _, err := app.Run(ctx); done <- err }()
	select {
	case snap := <-started:
		s := snap.Terminal.Native
		if s.Colors.Background != (ghostty.ColorRGB{R: 0x20, G: 0x30, B: 0x40}) || s.Colors.Foreground != (ghostty.ColorRGB{R: 0xd0, G: 0xc0, B: 0xb0}) {
			t.Errorf("child defaults fg=%+v bg=%+v", s.Colors.Foreground, s.Colors.Background)
		}
		if s.Colors.Palette[1] != (ghostty.ColorRGB{R: 0x12, G: 0x34, B: 0x56}) || s.Colors.Palette[200] != (ghostty.ColorRGB{R: 0x65, G: 0x43, B: 0x21}) {
			t.Error("child palette does not match outer palette")
		}
		if !s.Colors.CursorHasValue || s.Colors.Cursor != (ghostty.ColorRGB{R: 0xab, G: 0xcd, B: 0xef}) {
			t.Error("child cursor color does not match outer cursor")
		}
		if s.Cursor.VisualStyle != ghostty.CursorVisualStyleBar || s.Cursor.Blinking {
			t.Errorf("child cursor=%+v", s.Cursor)
		}
		if !s.Modes[ghostty.ModeDECCKM] {
			t.Error("child cursor-key default was not inherited")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case err := <-done:
		t.Fatalf("session ended before startup: %v", err)
	}
	awaitText(t, h, "child")
	h.mu.Lock()
	if h.err != nil {
		t.Errorf("outer harness error: %v", h.err)
	}
	outer, err := h.em.State()
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if got := outer.Cells[0].Style.Bg; got != (color.RGBA{R: 0x20, G: 0x30, B: 0x40, A: 255}) {
		t.Errorf("painted child background=%v", got)
	}
	if _, err := unix.Write(h.fd, []byte("q")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestSessionPaletteEncodingFollowsOuterColorProfile(t *testing.T) {
	red := ghostty.ColorRGB{R: 0xdc, G: 0x32, B: 0x2f}
	for _, tt := range []struct {
		name, colorTerm string
		want            ghostty.StyleColor
	}{
		{"256 colors", "", ghostty.StyleColor{Tag: ghostty.StyleColorPalette, Palette: 1}},
		{"true color", "truecolor", ghostty.StyleColor{Tag: ghostty.StyleColorRGB, RGB: red}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// The renderer detects its profile from the process environment.
			// This 256color TERM has no terminfo entry and names no terminal
			// that detection treats as true color, so only these variables
			// decide between ANSI256 and TrueColor.
			for key, value := range map[string]string{
				"TERM": "frame-256color", "COLORTERM": tt.colorTerm, "TMUX": "", "NO_COLOR": "",
				"CLICOLOR_FORCE": "", "TTY_FORCE": "", "WT_SESSION": "", "GOOGLE_CLOUD_SHELL": "",
			} {
				t.Setenv(key, value)
			}
			h := newHarness(t)
			h.mu.Lock()
			_, err := h.em.Write([]byte(fmt.Sprintf("\x1b]4;1;#%02x%02x%02x\x07", red.R, red.G, red.B)))
			h.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			cmd := exec.Command("sh", "-c", `printf '\033[31mpalette\033[0m'; read line`)
			go func() { _, err := New(cmd, struct{}{}).Terminal(h.slave, h.slave).Run(ctx); done <- err }()
			awaitText(t, h, "palette")
			h.mu.Lock()
			outer, err := h.em.State()
			h.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			x := slices.IndexFunc(outer.Cells, func(cell uv.Cell) bool { return cell.Content == "p" })
			if got := outer.NativeCells[x].Style.FgColor(); got != tt.want {
				t.Errorf("outer foreground for child SGR 31 = %+v; want %+v", got, tt.want)
			}
			if _, err := unix.Write(h.fd, []byte("\r")); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}

func TestInheritanceChild(t *testing.T) {
	if file := os.Getenv("FRAME_INHERITANCE_GRAPHEME_FILE"); file != "" {
		measureGraphemeLine(t, file)
		return
	}
	if os.Getenv("FRAME_INHERITANCE_CURSOR") == "1" {
		if _, err := term.MakeRaw(os.Stdin.Fd()); err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprint(os.Stdout, "\x1b[3 q\x1b]12;#123456\x07cursor changed"); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 256)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(buf[:n]), "q") {
				return
			}
		}
	}
	file := os.Getenv("FRAME_INHERITANCE_FILE")
	if file == "" {
		return
	}
	state, err := term.GetState(os.Stdin.Fd())
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(state.Termios)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
}

// graphemeLine puts a ZWJ family emoji between two letters. Grapheme
// clustering measures the emoji as one character two cells wide; wcwidth gives
// each of its three emoji two cells.
const graphemeLine = "a\U0001F468\u200d\U0001F469\u200d\U0001F467b"

// graphemeLineRow is the 1-based row measureGraphemeLine prints on. Below row
// 1 a cursor position report cannot be mistaken for a modified F3 key, which
// shares its CSI 1 ; Pm R form.
const graphemeLineRow = 2

// measureGraphemeLine prints graphemeLine as the child starts and again after a
// full reset (RIS), which restores the reset defaults of the child's modes.
// After each it requests the cursor position, then records the 0-based
// columns the terminal reported in file and prints "measured".
func measureGraphemeLine(t *testing.T, file string) {
	if _, err := term.MakeRaw(os.Stdin.Fd()); err != nil {
		t.Fatal(err)
	}
	framer := input.New()
	buf := make([]byte, 256)
	var columns []int
	for _, reset := range []string{"", ansi.ResetInitialState} {
		if _, err := fmt.Fprint(os.Stdout, reset+ansi.CursorPosition(1, graphemeLineRow)+graphemeLine+ansi.RequestCursorPositionReport); err != nil {
			t.Fatal(err)
		}
		column := -1
		for column < 0 {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				t.Fatal(err)
			}
			packets, err := framer.Feed(buf[:n])
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range packets {
				if report, ok := p.Event.(uv.CursorPositionEvent); ok {
					column = report.X
				}
			}
		}
		columns = append(columns, column)
	}
	data, err := json.Marshal(columns)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprint(os.Stdout, ansi.CursorPosition(1, graphemeLineRow+2)+"measured"); err != nil {
		t.Fatal(err)
	}
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(buf[:n]), "q") {
			return
		}
	}
}

// The session decides how the outer terminal measures text, and the child
// emulator must measure the same way from its start and after a reset, with or
// without inheritance. Otherwise the child reports and computes columns the
// screen does not show.
func TestSessionChildMeasuresTextAsTheOuterTerminalDoes(t *testing.T) {
	for _, inherit := range []bool{true, false} {
		for _, tt := range []struct {
			name   string
			report ansi.ModeSetting
			// wcwidth is the outer terminal's rule before the session;
			// grapheme is the rule the session measures with on it.
			wcwidth, grapheme bool
		}{
			{"set", ansi.ModeSet, false, true},
			{"reset", ansi.ModeReset, true, true},
			{"permanently set", ansi.ModePermanentlySet, false, true},
			{"permanently reset", ansi.ModePermanentlyReset, true, false},
		} {
			t.Run(fmt.Sprintf("%s/inherit=%v", tt.name, inherit), func(t *testing.T) {
				h := newTerminalHarness(t, harnessTerminal{cols: 40, rows: 12, cellWidth: harnessCellWidth, cellHeight: harnessCellHeight, wcwidth: tt.wcwidth})
				// The native emulator starts in the state the report under test
				// describes, but itself reports only switchable values.
				h.report(ansi.ModeUnicodeCore, ansi.ReportMode(ansi.ModeUnicodeCore, tt.report))
				file := filepath.Join(t.TempDir(), "columns.json")
				cmd := exec.Command(os.Args[0], "-test.run=^TestInheritanceChild$")
				cmd.Env = append(os.Environ(), "FRAME_INHERITANCE_GRAPHEME_FILE="+file)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				started := make(chan Snapshot, 1)
				done := make(chan error, 1)
				app := New(cmd, struct{}{}).Terminal(h.slave, h.slave).InheritTerminal(inherit).Observe(func(e Event) {
					if e.Kind == Started {
						started <- *e.Snapshot
					}
				})
				go func() { _, err := app.Run(ctx); done <- err }()
				select {
				case snap := <-started:
					if got := snap.Terminal.Native.Modes[ghostty.ModeGraphemeCluster]; got != tt.grapheme {
						t.Errorf("child grapheme clustering at start = %v, want %v", got, tt.grapheme)
					}
				case err := <-done:
					t.Fatalf("session ended before startup: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				awaitText(t, h, "measured")
				h.mu.Lock()
				outer, err := h.em.State()
				h.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				if got := outer.Modes[ghostty.ModeGraphemeCluster]; got != tt.grapheme {
					t.Errorf("outer grapheme clustering = %v, want %v", got, tt.grapheme)
				}
				cols := outer.Size.Cols
				row := outer.Cells[(graphemeLineRow-1)*cols : graphemeLineRow*cols]
				b := slices.IndexFunc(row, func(cell uv.Cell) bool { return cell.Content == "b" })
				if b < 0 {
					t.Fatalf("outer row %d does not show the line: %q", graphemeLineRow, h.text())
				}
				data, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				var columns []int
				if err := json.Unmarshal(data, &columns); err != nil {
					t.Fatal(err)
				}
				// The cursor follows the line where the outer terminal shows it.
				for i, when := range []string{"at start", "after RIS"} {
					if columns[i] != b+1 {
						t.Errorf("child cursor column after the line %s = %d; the outer terminal shows the line ending at %d", when, columns[i], b+1)
					}
				}
				if _, err := unix.Write(h.fd, []byte("q")); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			})
		}
	}
}

// The outer cursor follows its configured defaults, as Ghostty's does until a
// program overrides it. Restoration resets the cursor the child changed to
// those defaults instead of writing the reported values back as an override.
func TestSessionRendersAndRestoresChildCursor(t *testing.T) {
	bar, steady := ghostty.TerminalCursorStyleBar, false
	configured := ghostty.ColorRGB{R: 0xab, G: 0xcd, B: 0xef}
	for _, tt := range []struct {
		name  string
		color *ghostty.ColorRGB
	}{
		{"configured color", &configured},
		// Without a configured color, the terminal answers OSC 12 with its
		// foreground and has no cursor color of its own, so Ghostty draws the
		// cursor from the theme. Writing the report back would pin it.
		{"no configured color", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			defaults := &emulator.Profile{Cursor: tt.color, CursorStyle: &bar, CursorBlink: &steady}
			h := newTerminalHarness(t, harnessTerminal{cols: 40, rows: 12, cellWidth: harnessCellWidth, cellHeight: harnessCellHeight, defaults: defaults})
			cmd := exec.Command(os.Args[0], "-test.run=^TestInheritanceChild$")
			cmd.Env = append(os.Environ(), "FRAME_INHERITANCE_CURSOR=1")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := New(cmd, struct{}{}).Terminal(h.slave, h.slave).Run(ctx); done <- err }()
			awaitText(t, h, "cursor changed")
			h.mu.Lock()
			s, err := h.em.State()
			h.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if s.Cursor.VisualStyle != ghostty.CursorVisualStyleUnderline || !s.Cursor.Blinking || s.Colors.Cursor != (ghostty.ColorRGB{R: 0x12, G: 0x34, B: 0x56}) {
				t.Errorf("outer cursor did not reflect child: cursor=%+v color=%+v", s.Cursor, s.Colors.Cursor)
			}
			if _, err := unix.Write(h.fd, []byte("q")); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				h.mu.Lock()
				s, err = h.em.State()
				h.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				if !s.Alternate {
					if s.Cursor.VisualStyle != ghostty.CursorVisualStyleBar || s.Cursor.Blinking || s.Colors.CursorHasValue != (tt.color != nil) || tt.color != nil && s.Colors.Cursor != *tt.color {
						t.Errorf("cursor was not restored to the defaults: %+v color=%+v set=%v", s.Cursor, s.Colors.Cursor, s.Colors.CursorHasValue)
					}
					return
				}
				time.Sleep(time.Millisecond)
			}
			t.Fatal("alternate screen was not restored")
		})
	}
}

// cursorSequence matches the cursor appearance sequences the frame writes:
// DECSCUSR, OSC 12 and OSC 112.
var cursorSequence = regexp.MustCompile(`\x1b\[[0-9]* q|\x1b\]1?12(;[^\x07]*)?\x07`)

// An inherited cursor the child leaves alone is already the outer terminal's
// own appearance. Writing it back would turn the terminal's configured or
// theme-following cursor into an explicit override that outlives the session,
// so the frame writes only what the child changed and resets that to the
// terminal's defaults.
func TestSessionWritesOnlyCursorAppearanceTheChildChanged(t *testing.T) {
	style := 6
	rgb := ghostty.ColorRGB{R: 0xab, G: 0xcd, B: 0xef}
	resetStyle, resetColor := ansi.SetCursorStyle(0), ansi.ResetCursorColor
	for _, tt := range []struct {
		name            string
		steps           []string
		session, ending []string
	}{
		{name: "unchanged"},
		{name: "style changed", steps: []string{"\x1b[3 q"}, session: []string{ansi.SetCursorStyle(3)}, ending: []string{resetStyle}},
		{name: "color changed", steps: []string{"\x1b]12;#123456\x07"}, session: []string{ansi.SetCursorColor("#123456")}, ending: []string{resetColor}},
		{
			name:    "changed twice",
			steps:   []string{"\x1b[3 q\x1b]12;#123456\x07", "\x1b[4 q\x1b]12;#654321\x07"},
			session: []string{ansi.SetCursorStyle(3), ansi.SetCursorColor("#123456"), ansi.SetCursorStyle(4), ansi.SetCursorColor("#654321")},
			ending:  []string{resetStyle, resetColor},
		},
		{
			name:    "changed and reset by the child",
			steps:   []string{"\x1b[3 q\x1b]12;#123456\x07", "\x1b[0 q\x1b]112\x07"},
			session: []string{ansi.SetCursorStyle(3), ansi.SetCursorColor("#123456"), resetStyle, resetColor},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reported := &console{inherit: true, preferences: terminalPreferences{cursorStyle: &style, profile: emulator.Profile{Cursor: &rgb}}}
			cfg := repaintConfig{size: Size{Cols: 20, Rows: 6}, mode: ansi.ModeReset, host: reported.childOptions(emulator.Size{}).Profile}
			s, out, slave := newConfiguredRepaintSession(t, cfg, nil)
			s.console.preferences = reported.preferences
			// The session composes its first frame with the probed preferences.
			s.composed = false
			if err := s.render(false); err != nil {
				t.Fatal(err)
			}
			if frame := repaintOutput(t, out); !bytes.Contains(frame, []byte(ansi.SetModeSynchronizedOutput)) {
				t.Fatalf("first frame was not painted: %q", frame)
			}
			for _, step := range tt.steps {
				readRepaintChunk(t, s, slave, step)
				if err := s.render(false); err != nil {
					t.Fatal(err)
				}
			}
			painted := len(repaintOutput(t, out))
			if err := s.console.restore(); err != nil {
				t.Fatal(err)
			}
			output := string(repaintOutput(t, out))
			if got := cursorSequence.FindAllString(output[:painted], -1); !slices.Equal(got, tt.session) {
				t.Errorf("session cursor writes = %q, want %q", got, tt.session)
			}
			if got := cursorSequence.FindAllString(output[painted:], -1); !slices.Equal(got, tt.ending) {
				t.Errorf("restore cursor writes = %q, want %q", got, tt.ending)
			}
		})
	}
}

func TestSessionCanDisableTerminalInheritance(t *testing.T) {
	h := newHarness(t)
	h.mu.Lock()
	_, err := h.em.Write([]byte("\x1b]10;#112233\x07\x1b]11;#445566\x07\x1b[?1h"))
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan Snapshot, 1)
	done := make(chan error, 1)
	go func() {
		_, err := New(childCommand(t), struct{}{}).Terminal(h.slave, h.slave).InheritTerminal(false).Observe(func(e Event) {
			if e.Kind == Started {
				started <- *e.Snapshot
			}
		}).Run(ctx)
		done <- err
	}()
	select {
	case snap := <-started:
		s := snap.Terminal.Native
		if s.Colors.Background != (ghostty.ColorRGB{}) || s.Colors.Foreground != (ghostty.ColorRGB{R: 255, G: 255, B: 255}) || s.Modes[ghostty.ModeDECCKM] {
			t.Errorf("opt-out child inherited outer settings: %+v %+v", s.Colors.Background, s.Colors.Foreground)
		}
	case err := <-done:
		t.Fatalf("session failed to start: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	awaitText(t, h, "child")
	if _, err := unix.Write(h.fd, []byte("q")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestSessionInheritsPTYLineDiscipline(t *testing.T) {
	h := newHarness(t)
	state, err := term.GetState(h.slave.Fd())
	if err != nil {
		t.Fatal(err)
	}
	state.Cc[unix.VERASE] = '#'
	state.Lflag &^= unix.ECHO
	state.Iflag &^= unix.IXON
	if err := term.SetState(h.slave.Fd(), state); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "settings.json")
	cmd := exec.Command(os.Args[0], "-test.run=^TestInheritanceChild$")
	cmd.Env = append(os.Environ(), "FRAME_INHERITANCE_FILE="+file)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := New(cmd, struct{}{}).Terminal(h.slave, h.slave).Run(ctx)
	if err != nil || !result.ProcessState.Success() {
		t.Fatalf("child result=%+v error=%v", result, err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var got unix.Termios
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Cc[unix.VERASE] != '#' || got.Lflag&unix.ECHO != 0 || got.Iflag&unix.IXON != 0 {
		t.Fatalf("child PTY did not inherit erase/echo/flow-control: %+v", got)
	}
}
