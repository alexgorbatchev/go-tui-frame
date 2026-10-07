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
	ghostty "go.mitchellh.com/libghostty"
	"golang.org/x/sys/unix"
)

func newRepaintSession(t testing.TB, mode ansi.ModeSetting, draw func(DrawContext[string])) (*session[string], *os.File, *os.File) {
	t.Helper()
	return newRepaintSessionSize(t, Size{Cols: 20, Rows: 6}, mode, draw)
}

func newRepaintSessionSize(t testing.TB, size Size, mode ansi.ModeSetting, draw func(DrawContext[string])) (*session[string], *os.File, *os.File) {
	t.Helper()
	return newConfiguredRepaintSession(t, repaintConfig{size: size, mode: mode}, draw)
}

// repaintConfig describes the outer terminal a repaint session paints.
// host holds the preferences the startup probe reports; nil models
// InheritTerminal(false). A known colors profile replaces the one detected
// from the capture file, before the child emulator is created, as the
// session creates it after the console's renderer.
type repaintConfig struct {
	size   Size
	mode   ansi.ModeSetting
	host   *emulator.Profile
	colors colorprofile.Profile
}

func newConfiguredRepaintSession(t testing.TB, cfg repaintConfig, draw func(DrawContext[string])) (*session[string], *os.File, *os.File) {
	t.Helper()
	size := cfg.size
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
	// File.Fd can restore blocking mode on pollable files. Cache the
	// descriptor before SetNonblock, as the production session does.
	masterFD := int(master.Fd())
	if err := unix.SetNonblock(masterFD, true); err != nil {
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
	c := &console{entry: map[ansi.DECMode]ansi.ModeSetting{2026: cfg.mode}, applied: make(map[ansi.DECMode]bool)}
	c.attachRenderer(out, []string{"TERM=xterm-256color"})
	if cfg.colors != colorprofile.Unknown {
		c.setColorProfile(cfg.colors)
	}
	c.renderer.EnterAltScreen()
	em, err := emulator.New(emulator.Options{Size: emulator.Size{Cols: g.child.Dx(), Rows: g.child.Dy()}, Profile: cfg.host, OuterColorProfile: c.colorProfile})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(em.Close)
	s := &session[string]{frame: f, console: c, terminal: em, geometry: g, screen: uv.NewScreenBuffer(size.Cols, size.Rows), fd: masterFD, snapshot: Snapshot{Child: ChildSnapshot{PID: os.Getpid()}}}
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

func TestErasedBackgroundReachesOuterTerminal(t *testing.T) {
	draw := func(ctx DrawContext[string]) {
		ctx.View.SetCell(0, 0, &uv.Cell{Content: "H", Width: 1})
	}
	// The capture file cannot advertise the outer terminal's color support.
	cfg := repaintConfig{size: Size{Cols: 20, Rows: 6}, mode: ansi.ModeReset, colors: colorprofile.TrueColor}
	s, out, slave := newConfiguredRepaintSession(t, cfg, draw)
	outer, err := emulator.New(emulator.Options{Size: emulator.Size{Cols: 20, Rows: 6}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(outer.Close)
	before := 0
	for _, step := range []struct {
		sequence string
		bg       color.RGBA
	}{
		{"\x1b[48;2;10;20;30m\x1b[2K\x1b[2;1H\x1b[2K> prompt\x1b[0m", color.RGBA{R: 10, G: 20, B: 30, A: 255}},
		{"\x1b[H\x1b[2K\x1b[2;1H\x1b[2K", color.RGBA{A: 255}},
	} {
		readRepaintChunk(t, s, slave, step.sequence)
		if err := s.render(false); err != nil {
			t.Fatal(err)
		}
		output := repaintOutput(t, out)
		if _, err := outer.Write(output[before:]); err != nil {
			t.Fatal(err)
		}
		painted, err := outer.State()
		if err != nil {
			t.Fatal(err)
		}
		for y := 1; y <= 2; y++ {
			for x := range painted.Size.Cols {
				cell := painted.Cells[y*painted.Size.Cols+x]
				if cell.Style.Bg != step.bg {
					t.Fatalf("outer background at %d,%d = %#v; want %#v", x, y, cell.Style.Bg, step.bg)
				}
			}
		}
		if painted.Cells[0].Content != "H" || painted.Cells[0].Style.Bg == step.bg && step.bg.R != 0 {
			t.Fatal("child background overwrote the header")
		}
		before = len(output)
	}
}

func TestOSCForegroundRepaintsCleanRowsAndResets(t *testing.T) {
	// The capture file cannot advertise the outer terminal's color support.
	cfg := repaintConfig{size: Size{Cols: 20, Rows: 6}, mode: ansi.ModeReset, colors: colorprofile.TrueColor}
	s, out, slave := newConfiguredRepaintSession(t, cfg, nil)
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

// themedPalette stands in for host-reported entries: Solarized base02 and red
// for 0 and 1, a customized 200, all differing from xterm's defaults, and
// xterm's own value for 16.
var themedPalette = map[uint8]ghostty.ColorRGB{0: {R: 0x07, G: 0x36, B: 0x42}, 1: {R: 0xdc, G: 0x32, B: 0x2f}, 16: {}, 200: {R: 0x65, G: 0x43, B: 0x21}}

// outerPainting replays a repaint session's frames on a native outer terminal.
type outerPainting struct {
	s          *session[string]
	out, slave *os.File
	outer      *emulator.Terminal
	painted    int
}

func newOuterPainting(t *testing.T, colors colorprofile.Profile) *outerPainting {
	t.Helper()
	cfg := repaintConfig{size: Size{Cols: 20, Rows: 6}, mode: ansi.ModeReset, host: &emulator.Profile{Palette: themedPalette}, colors: colors}
	s, out, slave := newConfiguredRepaintSession(t, cfg, nil)
	outer, err := emulator.New(emulator.Options{Size: emulator.Size{Cols: 20, Rows: 6}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(outer.Close)
	// Paint the session's initial frame so each step sees only its own frame.
	initial := repaintOutput(t, out)
	if _, err := outer.Write(initial); err != nil {
		t.Fatal(err)
	}
	return &outerPainting{s: s, out: out, slave: slave, outer: outer, painted: len(initial)}
}

// paint repaints after child output and returns the outer terminal's
// foreground at cell x with the frame that produced it.
func (p *outerPainting) paint(t *testing.T, sequence string, x int) (ghostty.StyleColor, []byte) {
	t.Helper()
	readRepaintChunk(t, p.s, p.slave, sequence)
	if err := p.s.render(false); err != nil {
		t.Fatal(err)
	}
	output := repaintOutput(t, p.out)
	frame := output[p.painted:]
	p.painted = len(output)
	if _, err := p.outer.Write(frame); err != nil {
		t.Fatal(err)
	}
	painted, err := p.outer.State()
	if err != nil {
		t.Fatal(err)
	}
	return painted.NativeCells[x].Style.FgColor(), frame
}

func paletteForeground(i uint8) ghostty.StyleColor {
	return ghostty.StyleColor{Tag: ghostty.StyleColorPalette, Palette: i}
}

func rgbForeground(r, g, b uint8) ghostty.StyleColor {
	return ghostty.StyleColor{Tag: ghostty.StyleColorRGB, RGB: ghostty.ColorRGB{R: r, G: g, B: b}}
}

// paletteStep is child output and the outer foreground expected at cell x.
type paletteStep struct {
	sequence string
	x        int
	want     ghostty.StyleColor
}

func TestReportedPaletteOnDownsamplingTerminals(t *testing.T) {
	override := color.RGBA{R: 0x12, G: 0x34, B: 0x56, A: 255}
	custom := color.RGBA{R: 0x65, G: 0x43, B: 0x21, A: 255}
	for _, tt := range []struct {
		colors colorprofile.Profile
		steps  []paletteStep
	}{
		{colorprofile.ANSI256, []paletteStep{
			{"\x1b[31mX", 0, paletteForeground(1)},
			// A child override is explicit RGB, downsampled like any other color.
			{"\x1b]4;1;#123456\a", 0, paletteForeground(uint8(ansi.Convert256(override)))},
			{"\x1b]104;1\a", 0, paletteForeground(1)},
		}},
		{colorprofile.ANSI, []paletteStep{
			{"\x1b[31mX", 0, paletteForeground(1)},
			// Entry 200 keeps the host's RGB, which the renderer approximates
			// with its nearest 16-color entry; a fixed index table would ignore
			// the host's customization.
			{"\x1b[38;5;200mY", 1, paletteForeground(uint8(ansi.Convert16(custom)))},
		}},
	} {
		t.Run(tt.colors.String(), func(t *testing.T) {
			p := newOuterPainting(t, tt.colors)
			for _, step := range tt.steps {
				got, frame := p.paint(t, step.sequence, step.x)
				if got != step.want {
					t.Fatalf("outer foreground after %q = %+v; want %+v (frame %q)", step.sequence, got, step.want, frame)
				}
				// Without an extended foreground, palette entry 1 can only have
				// arrived as the basic SGR 31 form.
				if step.want == paletteForeground(1) && bytes.Contains(frame, []byte("38;")) {
					t.Fatalf("palette entry 1 after %q used an extended foreground: %q", step.sequence, frame)
				}
			}
		})
	}
}

func TestTrueColorTerminalReceivesResolvedPaletteRGB(t *testing.T) {
	// The collision cases pair a palette entry with a color whose xterm
	// default RGBA is the same; a palette index there would keep the earlier
	// color, because the renderer compares colors only by RGBA.
	for _, tt := range []struct {
		name  string
		steps []string
		x     int
		want  ghostty.StyleColor
	}{
		{"reported entry", []string{"\x1b[31mX"}, 0, rgbForeground(0xdc, 0x32, 0x2f)},
		{"child override", []string{"\x1b[31mX", "\x1b]4;1;#123456\a"}, 0, rgbForeground(0x12, 0x34, 0x56)},
		{"child reset", []string{"\x1b[31mX", "\x1b]4;1;#123456\a", "\x1b]104;1\a"}, 0, rgbForeground(0xdc, 0x32, 0x2f)},
		{"SGR 30 beside RGB black", []string{"\x1b[38;2;0;0;0mA\x1b[30mB"}, 1, rgbForeground(0x07, 0x36, 0x42)},
		{"RGB black replacing SGR 30", []string{"\x1b[30mX", "\x1b[H\x1b[38;2;0;0;0mX"}, 0, rgbForeground(0, 0, 0)},
		{"entry 1 redefined to its xterm value", []string{"\x1b[31mX", "\x1b]4;1;#800000\a"}, 0, rgbForeground(0x80, 0, 0)},
		{"38;5;16 beside SGR 30", []string{"\x1b[30mA\x1b[38;5;16mB"}, 1, rgbForeground(0, 0, 0)},
		{"38;5;16 replacing SGR 30", []string{"\x1b[30mX", "\x1b[H\x1b[38;5;16mX"}, 0, rgbForeground(0, 0, 0)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := newOuterPainting(t, colorprofile.TrueColor)
			var got ghostty.StyleColor
			var frame []byte
			for _, sequence := range tt.steps {
				got, frame = p.paint(t, sequence, tt.x)
			}
			if got != tt.want {
				t.Fatalf("outer foreground at %d = %+v; want %+v (last frame %q)", tt.x, got, tt.want, frame)
			}
		})
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
	for _, text := range []string{
		"\x1b]2;updated title\x07", "\x1b]7;file://host/new\x07", "\x1b[?2004h", "\x1b[?1049h",
		// Setting 1000 again changes only the active tracking mode.
		"\x1b[?1000h\x1b[?1002h\x1b[?1002l", "\x1b[?1000h",
	} {
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
	if _, err := c.probe(ctx, nil, capabilityTimeout); err != nil {
		t.Fatal(err)
	}
	if !c.switchable(2026) {
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
	if _, err := c.probe(ctx, nil, capabilityTimeout); err != nil {
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
