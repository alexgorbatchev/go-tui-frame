package frame

import (
	"context"
	"encoding/json"
	"fmt"
	"image/color"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
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

func TestSessionRendersAndRestoresChildCursor(t *testing.T) {
	h := newHarness(t)
	h.mu.Lock()
	_, err := h.em.Write([]byte("\x1b[6 q\x1b]12;#abcdef\x07"))
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
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
			if s.Cursor.VisualStyle != ghostty.CursorVisualStyleBar || s.Cursor.Blinking || s.Colors.Cursor != (ghostty.ColorRGB{R: 0xab, G: 0xcd, B: 0xef}) {
				t.Errorf("cursor was not restored: %+v %+v", s.Cursor, s.Colors.Cursor)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("alternate screen was not restored")
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
