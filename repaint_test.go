package frame

import (
	"bytes"
	"context"
	"image/color"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/alexgorbatchev/go-tui-frame/internal/emulator"
	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

func newRepaintSession(t testing.TB, mode ansi.ModeSetting, draw func(DrawContext[string])) (*session[string], *os.File, *os.File) {
	t.Helper()
	return newRepaintSessionSize(t, Size{Cols: 20, Rows: 6}, mode, draw)
}

func newRepaintSessionSize(t testing.TB, size Size, mode ansi.ModeSetting, draw func(DrawContext[string])) (*session[string], *os.File, *os.File) {
	t.Helper()
	if err := os.MkdirAll(".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	out, err := os.CreateTemp(".tmp", "repaint-*")
	if err != nil {
		t.Fatal(err)
	}
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, file := range []*os.File{out, master, slave} {
			if err := file.Close(); err != nil {
				t.Error(err)
			}
		}
		if err := os.Remove(out.Name()); err != nil {
			t.Error(err)
		}
	})
	if _, err := term.MakeRaw(slave.Fd()); err != nil {
		t.Fatal(err)
	}
	if err := unix.SetNonblock(int(master.Fd()), true); err != nil {
		t.Fatal(err)
	}
	f := New(exec.Command("sh"), "header")
	if draw != nil {
		f.Header(1, draw)
	}
	g, err := f.layout(size)
	if err != nil {
		t.Fatal(err)
	}
	em, err := emulator.New(emulator.Options{Size: emulator.Size{Cols: g.child.Dx(), Rows: g.child.Dy()}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(em.Close)
	c := &console{renderer: uv.NewTerminalRenderer(out, []string{"TERM=xterm-256color"}), entry: map[ansi.DECMode]ansi.ModeSetting{2026: mode}, applied: make(map[ansi.DECMode]bool)}
	c.renderer.EnterAltScreen()
	s := &session[string]{frame: f, console: c, terminal: em, geometry: g, screen: uv.NewScreenBuffer(size.Cols, size.Rows), fd: int(master.Fd()), snapshot: Snapshot{Child: ChildSnapshot{PID: os.Getpid()}}}
	if err := s.refresh(true); err != nil {
		t.Fatal(err)
	}
	return s, out, slave
}

func repaintOutput(t *testing.T, out *os.File) []byte {
	t.Helper()
	b, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readRepaintChunk(t testing.TB, s *session[string], slave *os.File, chunk string) {
	t.Helper()
	if _, err := slave.WriteString(chunk); err != nil {
		t.Fatal(err)
	}
	fds := []unix.PollFd{{Fd: int32(s.fd), Events: unix.POLLIN}}
	if n, err := unix.Poll(fds, 1000); err != nil || n == 0 {
		t.Fatalf("child chunk not readable: %d, %v", n, err)
	}
	if err := s.readChild(make([]byte, len(chunk))); err != nil {
		t.Fatal(err)
	}
}

func TestOSCForegroundRepaintsCleanRowsAndResets(t *testing.T) {
	s, out, slave := newRepaintSession(t, ansi.ModeReset, nil)
	// The capture file cannot advertise the outer terminal's color support.
	s.console.renderer.SetColorProfile(colorprofile.TrueColor)
	outer, err := emulator.New(emulator.Options{Size: emulator.Size{Cols: 20, Rows: 6}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(outer.Close)
	readRepaintChunk(t, s, slave, "one\r\ntwo")
	if err := s.render(false); err != nil {
		t.Fatal(err)
	}
	initial := repaintOutput(t, out)
	if _, err := outer.Write(initial); err != nil {
		t.Fatal(err)
	}
	before := len(initial)
	original := s.screen.CellAt(0, 0).Style.Fg
	for _, step := range []struct {
		sequence string
		color    color.Color
	}{
		{"\x1b]10;#123456\a", color.RGBA{R: 0x12, G: 0x34, B: 0x56, A: 255}},
		{"\x1b]110\a", original},
	} {
		readRepaintChunk(t, s, slave, step.sequence)
		if err := s.render(false); err != nil {
			t.Fatal(err)
		}
		output := repaintOutput(t, out)
		if len(output) == before {
			t.Fatal("color-only change did not repaint the outer terminal")
		}
		if _, err := outer.Write(output[before:]); err != nil {
			t.Fatal(err)
		}
		painted, err := outer.State()
		if err != nil {
			t.Fatal(err)
		}
		for y, content := range []string{"o", "t"} {
			cell := painted.Cells[y*20]
			if cell.Content != content || cell.Style.Fg != step.color {
				t.Fatalf("outer row %d after %q: %#v; want foreground %#v", y, step.sequence, cell, step.color)
			}
		}
		before = len(output)
	}
}

func TestChildRepaintCoalescesChunksAndFlushesWhileIdle(t *testing.T) {
	s, out, slave := newRepaintSession(t, ansi.ModeReset, nil)
	before := repaintOutput(t, out)
	for _, char := range "hello" {
		readRepaintChunk(t, s, slave, string(char))
	}
	deadline := s.renderDeadline
	readRepaintChunk(t, s, slave, "!")
	if !s.renderDeadline.Equal(deadline) {
		t.Fatal("continued child output postponed the existing frame deadline")
	}
	if got := repaintOutput(t, out); !bytes.Equal(got, before) {
		t.Fatal("child chunks were displayed before the frame deadline")
	}
	if timeout := s.pollTimeout(); timeout < 0 || timeout > 20 {
		t.Fatalf("pending repaint has no bounded poll deadline: %d", timeout)
	}
	time.Sleep(25 * time.Millisecond)
	if err := s.deadlines(); err != nil {
		t.Fatal(err)
	}
	if got := repaintOutput(t, out)[len(before):]; !bytes.Contains(got, []byte("hello!")) || bytes.Count(got, []byte("\x1b[?2026h")) != 1 {
		t.Fatalf("idle burst did not flush as one complete frame: %q", got)
	}
	if timeout := s.pollTimeout(); timeout != -1 {
		t.Fatalf("completed frame left an idle rendering timer: %d", timeout)
	}
}

func TestChildRepaintFlushesFinalOutputBeforeExit(t *testing.T) {
	s, out, slave := newRepaintSession(t, ansi.ModeReset, nil)
	before := repaintOutput(t, out)
	readRepaintChunk(t, s, slave, "final")
	s.waited, s.ptyEOF = true, true
	if err := s.loop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := repaintOutput(t, out)[len(before):]; !bytes.Contains(got, []byte("final")) {
		t.Fatalf("exit dropped the pending frame: %q", got)
	}
}

func TestChildRepliesDoNotWaitForRepaint(t *testing.T) {
	s, out, slave := newRepaintSession(t, ansi.ModeReset, nil)
	before := repaintOutput(t, out)
	readRepaintChunk(t, s, slave, "\x1b[6n")
	if len(s.queue) != 1 || string(s.queue[0].bytes) != "\x1b[1;1R" {
		t.Fatalf("cursor-position reply was delayed: %#v", s.queue)
	}
	if !bytes.Equal(repaintOutput(t, out), before) {
		t.Fatal("query forced an intermediate repaint")
	}
}

func TestChildTextDoesNotRedrawRegionsOrResampleProcesses(t *testing.T) {
	draws := 0
	s, _, _ := newRepaintSession(t, ansi.ModeReset, func(ctx DrawContext[string]) {
		draws++
		uv.NewStyledString(ctx.Term.Terminal.Title).Draw(ctx.View, ctx.View.Bounds())
	})
	observed := s.snapshot.Child.PTY.ObservedAt
	for _, text := range []string{"x", "y", "\x1b[H", "\x1b[?25l"} {
		if _, err := s.terminal.Write([]byte(text)); err != nil {
			t.Fatal(err)
		}
		if err := s.refresh(false); err != nil {
			t.Fatal(err)
		}
	}
	if draws != 1 || !s.snapshot.Child.PTY.ObservedAt.Equal(observed) {
		t.Fatalf("text/cursor updates redrew chrome or resampled processes: draws=%d", draws)
	}
	for _, text := range []string{"\x1b]2;updated title\x07", "\x1b]7;file://host/new\x07", "\x1b[?2004h", "\x1b[?1049h"} {
		previous := draws
		if _, err := s.terminal.Write([]byte(text)); err != nil {
			t.Fatal(err)
		}
		if err := s.refresh(false); err != nil {
			t.Fatal(err)
		}
		if draws != previous+1 {
			t.Fatalf("metadata change did not redraw regions: %q", text)
		}
	}
}

func TestOuterRepaintUsesNegotiatedSynchronizedOutput(t *testing.T) {
	for _, tt := range []struct {
		name string
		mode ansi.ModeSetting
		want bool
	}{{"supported", ansi.ModeReset, true}, {"unsupported", ansi.ModeNotRecognized, false}, {"permanently disabled", ansi.ModePermanentlyReset, false}} {
		t.Run(tt.name, func(t *testing.T) {
			_, out, _ := newRepaintSession(t, tt.mode, nil)
			got := string(repaintOutput(t, out))
			begin, end := strings.Index(got, "\x1b[?2026h"), strings.LastIndex(got, "\x1b[?2026l")
			if tt.want {
				if begin < 0 || end <= begin || !strings.Contains(got[begin:end], "\x1b[2J") {
					t.Fatalf("screen update is not enclosed in synchronized output: %q", got)
				}
			} else if begin >= 0 || end >= 0 {
				t.Fatalf("unsupported mode was used: %q", got)
			}
		})
	}
}

func TestConsoleProbesSynchronizedOutput(t *testing.T) {
	h := newHarness(t)
	fd, device, _, err := inspectConsole(h.slave, h.slave)
	if err != nil {
		t.Fatal(err)
	}
	c, err := acquireConsole(h.slave, h.slave, fd, device)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.restore(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := c.probe(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if !c.supports(2026) {
		t.Fatal("outer synchronized-output support was not negotiated")
	}
}

func TestConsoleRestoresSynchronizedEntryHold(t *testing.T) {
	h := newHarness(t)
	h.mu.Lock()
	_, err := h.em.Write([]byte("\x1b[?2026h"))
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	fd, device, _, err := inspectConsole(h.slave, h.slave)
	if err != nil {
		t.Fatal(err)
	}
	c, err := acquireConsole(h.slave, h.slave, fd, device)
	if err != nil {
		t.Fatal(err)
	}
	restored := false
	t.Cleanup(func() {
		if !restored {
			if err := c.restore(); err != nil {
				t.Error(err)
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := c.probe(ctx, nil); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		name      string
		held, alt bool
		run       func() error
	}{{"enter", false, true, c.enter}, {"restore", true, false, c.restore}} {
		if err := step.run(); err != nil {
			t.Fatal(err)
		}
		restored = step.name == "restore"
		for {
			h.mu.Lock()
			state, err := h.em.State()
			h.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if state.Held == step.held && state.Alternate == step.alt {
				break
			}
			if ctx.Err() != nil {
				t.Fatalf("%s did not preserve synchronized-output ownership: held=%v, alt=%v", step.name, state.Held, state.Alternate)
			}
			time.Sleep(time.Millisecond)
		}
	}
}
