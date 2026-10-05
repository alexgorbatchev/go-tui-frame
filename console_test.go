package frame

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexgorbatchev/go-tui-frame/internal/emulator"
	"github.com/alexgorbatchev/go-tui-frame/internal/input"
	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"
	ghostty "go.mitchellh.com/libghostty"
)

func TestCaptureNegotiatesDistinctKeysAndRestoresOuterMode(t *testing.T) {
	for _, tt := range []struct {
		capture bool
		child   ghostty.KittyKeyFlags
		want    ghostty.KittyKeyFlags
	}{{false, 0, 0}, {true, 0, ghostty.KittyKeyDisambiguate}, {false, ghostty.KittyKeyReportEvents, ghostty.KittyKeyReportEvents}, {true, ghostty.KittyKeyReportEvents, ghostty.KittyKeyReportEvents | ghostty.KittyKeyDisambiguate}} {
		t.Run(fmt.Sprintf("capture=%v/child=%d", tt.capture, tt.child), func(t *testing.T) {
			h := newHarness(t)
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
			c.capture = tt.capture
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if _, err := c.probe(ctx, nil); err != nil {
				t.Fatal(err)
			}
			if !c.kittySupported {
				t.Fatal("native test outer terminal did not report Kitty support")
			}
			child, err := emulator.New(emulator.Options{Size: emulator.Size{Cols: 10, Rows: 5}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(child.Close)
			if _, err := child.Write([]byte(fmt.Sprintf("\x1b[>%du", tt.child))); err != nil {
				t.Fatal(err)
			}
			state, err := child.State()
			if err != nil {
				t.Fatal(err)
			}
			if err := c.enter(); err != nil {
				t.Fatal(err)
			}
			if err := c.syncInput(state); err != nil {
				t.Fatal(err)
			}
			// A marker after the mode write proves the native outer parsed it.
			if _, err := c.renderer.WriteString("\x1b]2;capture-mode-test\x07"); err != nil {
				t.Fatal(err)
			}
			if err := c.renderer.Flush(); err != nil {
				t.Fatal(err)
			}
			for {
				h.mu.Lock()
				outer, err := h.em.State()
				h.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				if outer.Title == "capture-mode-test" {
					if outer.KittyKeyboardFlags != tt.want {
						t.Fatalf("outer flags=%d, want %d", outer.KittyKeyboardFlags, tt.want)
					}
					break
				}
				if ctx.Err() != nil {
					t.Fatal("mode marker did not arrive", ctx.Err())
				}
				time.Sleep(time.Millisecond)
			}
			if err := c.restore(); err != nil {
				t.Fatal(err)
			}
			restored = true
			for {
				h.mu.Lock()
				outer, err := h.em.State()
				h.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				if !outer.Alternate {
					if outer.KittyKeyboardFlags != 0 {
						t.Fatalf("restored flags=%d", outer.KittyKeyboardFlags)
					}
					break
				}
				if ctx.Err() != nil {
					t.Fatal("outer mode was not restored", ctx.Err())
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}

func TestConsoleRestoresObservedEntryModes(t *testing.T) {
	h := newHarness(t)
	h.mu.Lock()
	if _, err := h.em.Write([]byte("\x1b[?1;1004;1006;2004h\x1b[>4;1m")); err != nil {
		h.mu.Unlock()
		t.Fatal(err)
	}
	h.mu.Unlock()
	fd, device, _, err := inspectConsole(h.slave, h.slave)
	if err != nil {
		t.Fatal(err)
	}
	c, err := acquireConsole(h.slave, h.slave, fd, device)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := c.probe(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.enter(); err != nil {
		t.Fatal(err)
	}
	for _, m := range []ansi.DECMode{1, 1004, 1006, 2004} {
		if err := c.setMode(m, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.restore(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		s, err := h.em.State()
		h.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if s.Modes[ghostty.ModeDECCKM] && s.Modes[ghostty.ModeFocusEvent] && s.Modes[ghostty.ModeSGRMouse] && s.Modes[ghostty.ModeBracketedPaste] && !s.Alternate {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("entry modes were not restored after alternate-screen exit")
}

func TestConsolePreservesOnlySolicitedReplies(t *testing.T) {
	c := &console{pending: map[ansi.DECMode]bool{1049: true}, entry: make(map[ansi.DECMode]ansi.ModeSetting), applied: make(map[ansi.DECMode]bool)}
	packets, err := input.New().Feed([]byte("\x1b[?25;1$y\x1b[?1049;2$y\x1b[?1049;1$y"))
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range packets {
		if got := c.consumeReply(p); got != (i == 1) {
			t.Fatalf("packet %d consumed = %v", i, got)
		}
	}
}

// reportModes decodes DECRPM replies through the probe's reply path, so the
// console records them exactly as it does during negotiation.
func reportModes(t *testing.T, c *console, reports map[ansi.DECMode]ansi.ModeSetting) {
	t.Helper()
	c.pending = make(map[ansi.DECMode]bool, len(reports))
	var replies string
	for m, v := range reports {
		c.pending[m] = true
		replies += ansi.ReportMode(m, v)
	}
	packets, err := input.New().Feed([]byte(replies))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range packets {
		if !c.consumeReply(p) {
			t.Fatalf("mode report %q was not consumed", p.Raw)
		}
	}
	if len(c.pending) != 0 {
		t.Fatalf("mode reports were not decoded: %v", c.pending)
	}
}

func TestConsoleKeepsReportedGraphemeWidth(t *testing.T) {
	const enable, disable = "\x1b[?2027h", "\x1b[?2027l"
	for _, tt := range []struct {
		name              string
		report            ansi.ModeSetting
		grapheme, enables bool
	}{
		{"switchable", ansi.ModeReset, true, true},
		{"permanently set", ansi.ModePermanentlySet, true, false},
		{"permanently reset", ansi.ModePermanentlyReset, false, false},
		{"unrecognized", ansi.ModeNotRecognized, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, out, _ := newRepaintSession(t, ansi.ModeReset, nil)
			c := s.console
			reportModes(t, c, map[ansi.DECMode]ansi.ModeSetting{2027: tt.report})
			// The child emulator is created before the session enters the screen.
			if got := c.childOptions(c.childSize(s.geometry)).GraphemeWidth; got != tt.grapheme {
				t.Errorf("child emulator grapheme width = %v, want %v", got, tt.grapheme)
			}
			before := len(repaintOutput(t, out))
			if err := c.enter(); err != nil {
				t.Fatal(err)
			}
			written := string(repaintOutput(t, out)[before:])
			if tt.enables {
				if strings.LastIndex(written, enable) <= strings.LastIndex(written, disable) {
					t.Errorf("enter did not leave grapheme width on: %q", written)
				}
			} else if strings.Contains(written, enable) || strings.Contains(written, disable) {
				t.Errorf("enter switched a mode the terminal cannot switch: %q", written)
			}
			if c.applied[2027] != tt.grapheme {
				t.Errorf("applied grapheme width = %v, want %v", c.applied[2027], tt.grapheme)
			}
			if err := s.applyGeometry(s.geometry); err != nil {
				t.Fatal(err)
			}
			want := ansi.WcWidth
			if tt.grapheme {
				want = ansi.GraphemeWidth
			}
			if s.screen.Method != want {
				t.Errorf("outer screen width method = %v, want %v", s.screen.Method, want)
			}
		})
	}
}

func TestConsoleKeepsPermanentInputModes(t *testing.T) {
	type modes = map[ansi.DECMode]ansi.ModeSetting
	for _, tt := range []struct {
		name            string
		reports         modes
		child           string
		written, absent []string
		applied         map[ansi.DECMode]bool
		cursor, pixels  bool
		outer, routed   string
		// unmeasured cases start before the cell-size reply arrives. pixelless
		// cases resize to a winsize without pixels before outer is routed. late
		// sequences must reach the outer terminal in order after the child
		// chunk. routeErr is the error routing outer must end with.
		unmeasured, pixelless bool
		late                  []string
		routeErr              string
	}{
		{
			name: "cursor keys permanently reset", reports: modes{1: ansi.ModePermanentlyReset},
			child: "\x1b[?1h", absent: []string{"\x1b[?1h"}, applied: map[ansi.DECMode]bool{1: false},
			outer: "\x1b[A", routed: "\x1bOA",
		},
		{
			name: "cursor keys permanently set", reports: modes{1: ansi.ModePermanentlySet},
			child: "\x1b[?1l", absent: []string{"\x1b[?1l"}, applied: map[ansi.DECMode]bool{1: true}, cursor: true,
			outer: "\x1bOA", routed: "\x1b[A",
		},
		{
			name: "pixel mouse switchable", reports: modes{1006: ansi.ModeReset, 1016: ansi.ModeReset},
			child: "\x1b[?1000;1016h", written: []string{"\x1b[?1016h"}, absent: []string{"\x1b[?1006h"},
			applied: map[ansi.DECMode]bool{1006: false, 1016: true}, pixels: true,
			outer: "\x1b[<0;61;91M", routed: "\x1b[<0;61;91M",
		},
		{
			name: "pixel mouse permanently reset", reports: modes{1006: ansi.ModeReset, 1016: ansi.ModePermanentlyReset},
			child: "\x1b[?1000;1016h", written: []string{"\x1b[?1006h"}, absent: []string{"\x1b[?1016h"},
			applied: map[ansi.DECMode]bool{1006: true, 1016: false},
			// Unsupported conversion fails explicitly: a cell report cannot
			// supply the pixel precision the child asked for.
			outer: "\x1b[<0;7;5M", routeErr: "child pixel mouse requires pixel input from the outer terminal",
		},
		{
			name: "pixel mouse permanently set", reports: modes{1006: ansi.ModeReset, 1016: ansi.ModePermanentlySet},
			child: "\x1b[?1000;1006h", written: []string{"\x1b[?1006h"}, absent: []string{"\x1b[?1016l"},
			applied: map[ansi.DECMode]bool{1006: true, 1016: true}, pixels: true,
			outer: "\x1b[<0;61;91M", routed: "\x1b[<0;7;5M",
		},
		{
			name: "pixel mouse permanently set without measured cells", unmeasured: true,
			reports: modes{1000: ansi.ModeReset, 1006: ansi.ModeReset, 1016: ansi.ModePermanentlySet},
			child:   "\x1b[?1000;1006h", absent: []string{"\x1b[?1000h", "\x1b[?1016l"},
			applied: map[ansi.DECMode]bool{1000: false, 1016: true}, pixels: true,
			outer: "\x1b[6;20;10t\x1b[<0;61;91M", late: []string{"\x1b[?1000h"}, routed: "\x1b[<0;7;5M",
		},
		{
			// A report the terminal sent before it applied the resize arrives
			// between the resize and the cell-size reply.
			name: "pixel mouse permanently set after pixel-less resize", pixelless: true,
			reports: modes{1000: ansi.ModeReset, 1006: ansi.ModeReset, 1016: ansi.ModePermanentlySet},
			child:   "\x1b[?1000;1006h", written: []string{"\x1b[?1000h"}, absent: []string{"\x1b[?1016l"},
			applied: map[ansi.DECMode]bool{1000: true, 1016: true}, pixels: true,
			outer: "\x1b[<0;61;91M\x1b[6;20;10t\x1b[<0;61;91M", late: []string{"\x1b[16t", "\x1b[?1000l", "\x1b[?1000h"},
			routed: "\x1b[<0;7;5M",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, out, slave := newRepaintSession(t, ansi.ModeReset, nil)
			c := s.console
			if !tt.unmeasured {
				c.cellWidth, c.cellHeight = harnessCellWidth, harnessCellHeight
			}
			// The probe leaves the cell-size query pending until a reply arrives.
			c.cellPending = tt.unmeasured
			router, err := newInputRouter(s.terminal, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(router.close)
			s.router = router
			reportModes(t, c, tt.reports)
			before := len(repaintOutput(t, out))
			// The console's cell size reaches the child emulator, as after a reply.
			if err := s.applyGeometry(s.geometry); err != nil {
				t.Fatal(err)
			}
			readRepaintChunk(t, s, slave, tt.child)
			written := string(repaintOutput(t, out)[before:])
			for _, seq := range tt.written {
				if !strings.Contains(written, seq) {
					t.Errorf("outer terminal did not receive %q: %q", seq, written)
				}
			}
			for _, seq := range tt.absent {
				if strings.Contains(written, seq) {
					t.Errorf("outer terminal received %q: %q", seq, written)
				}
			}
			for m, want := range tt.applied {
				if c.applied[m] != want {
					t.Errorf("applied[%d] = %v, want %v", m, c.applied[m], want)
				}
			}
			if tt.pixelless {
				resizeWithoutPixels(t, s)
			}
			queued := len(s.queue)
			packets, err := input.New().Feed([]byte(tt.outer))
			if err != nil {
				t.Fatal(err)
			}
			var routeErr error
			for _, p := range packets {
				if routeErr = s.route(p); routeErr != nil {
					break
				}
			}
			if tt.routeErr == "" && routeErr != nil {
				t.Fatalf("routing %q failed: %v", tt.outer, routeErr)
			}
			if tt.routeErr != "" && (routeErr == nil || !strings.Contains(routeErr.Error(), tt.routeErr)) {
				t.Errorf("routing %q returned %v, want error %q", tt.outer, routeErr, tt.routeErr)
			}
			late := string(repaintOutput(t, out)[before+len(written):])
			for rest, i := late, 0; i < len(tt.late); i++ {
				at := strings.Index(rest, tt.late[i])
				if at < 0 {
					t.Errorf("outer terminal did not receive %q after %q while routing %q: %q", tt.late[i], tt.late[:i], tt.outer, late)
					break
				}
				rest = rest[at+len(tt.late[i]):]
			}
			if host := s.router.host; host.ApplicationCursor != tt.cursor || host.MousePixels != tt.pixels {
				t.Errorf("host profile cursor=%v pixels=%v, want cursor=%v pixels=%v", host.ApplicationCursor, host.MousePixels, tt.cursor, tt.pixels)
			}
			var routed []byte
			for _, p := range s.queue[queued:] {
				routed = append(routed, p.bytes[p.offset:]...)
			}
			if string(routed) != tt.routed {
				t.Errorf("child received %q for outer %q, want %q", routed, tt.outer, tt.routed)
			}
		})
	}
}

// resizeWithoutPixels resizes the session as a SIGWINCH does when the outer
// winsize carries cells but no pixel dimensions.
func resizeWithoutPixels(t *testing.T, s *session[string]) {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, file := range []*os.File{master, slave} {
			if err := file.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	if err := pty.Setsize(slave, &pty.Winsize{Cols: uint16(s.geometry.outer.Dx()), Rows: uint16(s.geometry.outer.Dy())}); err != nil {
		t.Fatal(err)
	}
	s.console.fd = int(slave.Fd())
	if err := s.resize(); err != nil {
		t.Fatal(err)
	}
}

func TestConsoleAppliesAndRestoresReportedModifyOtherKeys(t *testing.T) {
	h := newHarness(t)
	h.mu.Lock()
	if _, err := h.em.Write([]byte(ansi.SetModifyOtherKeys2)); err != nil {
		h.mu.Unlock()
		t.Fatal(err)
	}
	h.mu.Unlock()
	fd, device, _, err := inspectConsole(h.slave, h.slave)
	if err != nil {
		t.Fatal(err)
	}
	c, err := acquireConsole(h.slave, h.slave, fd, device)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := c.probe(ctx, nil); err != nil {
		t.Fatal(err)
	}
	// libghostty-vt does not answer this optional query. Decode a reported
	// xterm response, matching the native endpoint's verified entry state.
	packets, err := input.New().Feed([]byte("\x1b[>4;2m"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.consumeReply(packets[0]) {
		t.Fatal("reported modifyOtherKeys entry value was not accepted")
	}
	if err := c.enter(); err != nil {
		t.Fatal(err)
	}
	if err := c.syncInput(NativeState{}); err != nil {
		t.Fatal(err)
	}
	if err := c.renderer.Flush(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	disabled := false
	for time.Now().Before(deadline) {
		h.mu.Lock()
		s, err := h.em.State()
		h.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if !s.ModifyOtherKeys2 {
			disabled = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !disabled {
		t.Fatal("outer modifyOtherKeys did not match child legacy mode")
	}
	// Select the observed MOK2 profile without relying on Kitty negotiation.
	// The native outer's mode agrees with the solicited report decoded above.
	c.capture, c.kittySupported = true, false
	if err := c.syncInput(NativeState{}); err != nil {
		t.Fatal(err)
	}
	if err := c.renderer.Flush(); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(time.Second)
	enabled := false
	for time.Now().Before(deadline) {
		h.mu.Lock()
		s, err := h.em.State()
		h.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if s.ModifyOtherKeys2 {
			enabled = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !enabled {
		t.Fatal("capture did not select the reported modifyOtherKeys fallback")
	}
	if err := c.restore(); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		s, err := h.em.State()
		h.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if s.ModifyOtherKeys2 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("outer modifyOtherKeys entry mode was not restored")
}

func TestProbeKeepsPasteFramingAcrossSessionHandoff(t *testing.T) {
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
	var once sync.Once
	writeErr := make(chan error, 1)
	events := newEventDispatcher(allEvents, sessionCancel(t), func(e Event) {
		if e.Kind == OuterInput && strings.Contains(string(e.Bytes), "\x1b[?1049;2$y") {
			once.Do(func() { writeErr <- h.writeReplies([]byte("\x1b[200~before")) })
		}
	})
	defer events.close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := c.probe(ctx, events); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-writeErr:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("paste was not injected after terminal capability replies")
	}
	packets, err := c.framer.Feed([]byte("\x11after\x1b[201~"))
	if err != nil {
		t.Fatal(err)
	}
	if len(packets) != 2 || !packets[0].Paste || string(packets[0].Raw) != "\x11after" {
		t.Fatalf("paste handoff reclassified input: %#v", packets)
	}
}
