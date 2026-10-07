package frame

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexgorbatchev/go-tui-frame/v2/internal/emulator"
	"github.com/alexgorbatchev/go-tui-frame/v2/internal/input"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"
	ghostty "go.mitchellh.com/libghostty"
	"golang.org/x/sys/unix"
)

// With Capture set, the outer keyboard runs exactly the child's modes: the
// child's Kitty flags where the terminal reported Kitty support and none
// otherwise, and the child's modifyOtherKeys level where the terminal reported
// that mode. Restoration returns the terminal to its entry modes.
func TestCaptureMirrorsChildKeyboardModes(t *testing.T) {
	for _, tt := range []struct {
		name, modes string
		kitty       bool
		flags       ghostty.KittyKeyFlags
		modify2     bool
	}{
		{name: "legacy child", kitty: true},
		{name: "disambiguating child", modes: "\x1b[>1u", kitty: true, flags: ghostty.KittyKeyDisambiguate},
		{name: "event-reporting child", modes: "\x1b[>2u", kitty: true, flags: ghostty.KittyKeyReportEvents},
		{name: "legacy child without Kitty support"},
		{name: "Kitty child without Kitty support", modes: "\x1b[>1u"},
		{name: "modifyOtherKeys child without Kitty support", modes: "\x1b[>4;2m", modify2: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			if !tt.kitty {
				h.withholdKittyKeyboard()
			}
			// libghostty-vt does not answer this optional query.
			h.answer(ansi.QueryModifyOtherKeys, "\x1b[>4;0m")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			child := exec.Command("sh", "-c", `printf '%sready' "$1"; exec sleep 30`, "sh", tt.modes)
			app := New(child, struct{}{}).Terminal(h.slave, h.slave).Capture(func(Input) Disposition { return Pass })
			done := startRun(ctx, app)
			// The session writes the outer modes for child output before it paints
			// that output, so they are in place once the text shows.
			awaitText(t, h, "ready")
			h.mu.Lock()
			outer, err := h.em.State()
			h.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if outer.KittyKeyboardFlags != tt.flags || outer.ModifyOtherKeys2 != tt.modify2 {
				t.Errorf("outer Kitty flags=%d modifyOtherKeys2=%v, want %d and %v", outer.KittyKeyboardFlags, outer.ModifyOtherKeys2, tt.flags, tt.modify2)
			}
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("Run did not return after cancellation")
			}
			awaitOuter(t, h, "its entry keyboard modes", func(s emulator.State) bool {
				return !s.Alternate && s.KittyKeyboardFlags == 0 && !s.ModifyOtherKeys2
			})
		})
	}
}

// A child's Kitty keyboard query (CSI ? u) gets no reply while the outer
// terminal has not reported Kitty support: the child would enable a protocol
// the terminal never sends. Replies to the child's other queries in the same
// output still arrive, and a Kitty reply the terminal sends after the startup
// probe makes later queries answered. A dropped reply emits no Protocol event.
func TestKittyKeyboardQueryAnsweredOnlyWithOuterSupport(t *testing.T) {
	const da1, decrpm, kittyReply = "\x1b[?62;22c", "\x1b[?7;1$y", "\x1b[?0u"
	for _, tt := range []struct {
		name  string
		kitty bool
	}{{"reported at startup", true}, {"reported late", false}} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			if !tt.kitty {
				h.withholdKittyKeyboard()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var (
				mu      sync.Mutex
				replies []byte
				// kittyEffects counts Protocol events for a Kitty reply.
				kittyEffects int
			)
			child := exec.Command("sh", "-c", "printf '\\033[?u\\033[c\\033[?7$p'; read line; printf '\\033[?u\\033[c'; exec sleep 30")
			app := New(child, struct{}{}).Terminal(h.slave, h.slave).ObserveEvents([]EventKind{ChildInput, Protocol}, func(e Event) {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case e.Kind == ChildInput && e.Origin == "terminal-reply":
					replies = append(replies, e.Bytes...)
				case e.Kind == Protocol && e.Effect.Kind == emulator.Reply && string(e.Effect.Bytes) == kittyReply:
					kittyEffects++
				}
			})
			done := startRun(ctx, app)
			awaitReplies := func(what string, ready func(string) bool) string {
				t.Helper()
				for {
					mu.Lock()
					got := string(replies)
					mu.Unlock()
					if ready(got) {
						return got
					}
					if ctx.Err() != nil {
						t.Fatalf("child did not receive %s: %q", what, got)
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			first := awaitReplies("its first replies", func(got string) bool { return strings.Contains(got, decrpm) })
			if !strings.Contains(first, da1) {
				t.Errorf("first replies %q lack the DA1 reply %q", first, da1)
			}
			if answered := strings.Contains(first, kittyReply); answered != tt.kitty {
				t.Errorf("first replies %q answer the Kitty query = %v, want %v", first, answered, tt.kitty)
			}
			// Observers receive each Protocol event before the write it schedules.
			mu.Lock()
			firstEffects := kittyEffects
			mu.Unlock()
			if want := map[bool]int{false: 0, true: 1}[tt.kitty]; firstEffects != want {
				t.Errorf("first queries emitted %d Protocol events for a Kitty reply, want %d", firstEffects, want)
			}
			next := []byte("x\n")
			if !tt.kitty {
				// The terminal answers the frame's startup query after the probe.
				next = append([]byte(kittyReply), next...)
			}
			if _, err := unix.Write(h.fd, next); err != nil {
				t.Fatal(err)
			}
			all := awaitReplies("its second replies", func(got string) bool { return strings.Count(got, da1) == 2 })
			if second := all[len(first):]; !strings.Contains(second, kittyReply) {
				t.Errorf("second replies %q do not answer the Kitty query", second)
			}
			mu.Lock()
			secondEffects := kittyEffects - firstEffects
			mu.Unlock()
			if secondEffects != 1 {
				t.Errorf("second query emitted %d Protocol events for a Kitty reply, want 1", secondEffects)
			}
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("Run did not return after cancellation")
			}
		})
	}
}

// A Kitty reply that misses the probe deadline arrives during the session,
// where the frame consumes it and starts using the reported support. Ghostty
// keeps a Kitty keyboard stack per screen and reuses the alternate screen, so
// flags the session leaves there apply to the next full-screen program.
func TestLateKittyReplyLeavesOuterKeyboardAsFound(t *testing.T) {
	const marker, seeded = "late-kitty-reply", "seeded-alternate-screen"
	// An earlier program left its own flags on the outer alternate screen. They
	// must survive the session, which a reset to 0 would not do.
	const found = ghostty.KittyKeyReportAlternates
	h := newHarness(t)
	seed := ansi.SetModeAltScreenSaveCursor + ansi.KittyKeyboard(int(found), 1) + ansi.ResetModeAltScreenSaveCursor + ansi.SetWindowTitle(seeded)
	if _, err := h.slave.WriteString(seed); err != nil {
		t.Fatal(err)
	}
	awaitOuter(t, h, "the seeded alternate screen", func(got emulator.State) bool {
		return got.Title == seeded
	})
	s := newProbedSession(t, h)
	c := s.console
	if !c.kittySupported {
		t.Fatal("native test outer terminal did not report Kitty support")
	}
	// Reconstruct the state the probe leaves at its deadline when the outer
	// terminal has not answered the Kitty query yet.
	c.kittyPending, c.kittySupported = true, false
	if err := c.enter(); err != nil {
		t.Fatal(err)
	}
	// The child requests Kitty disambiguation before the late reply arrives.
	if _, err := s.terminal.Write([]byte(ansi.PushKittyKeyboard(int(ghostty.KittyKeyDisambiguate)))); err != nil {
		t.Fatal(err)
	}
	if err := s.terminal.InputState(&s.inputState); err != nil {
		t.Fatal(err)
	}
	if err := s.route(decodePacket(t, "\x1b[?1u")); err != nil {
		t.Fatal(err)
	}
	var routed []byte
	for _, p := range s.queue {
		routed = append(routed, p.bytes[p.offset:]...)
	}
	if len(routed) != 0 {
		t.Errorf("child received %q, want the late Kitty reply consumed", routed)
	}
	// A marker after the mode writes proves the native outer parsed them.
	if _, err := c.renderer.WriteString(ansi.SetWindowTitle(marker)); err != nil {
		t.Fatal(err)
	}
	if err := c.renderer.Flush(); err != nil {
		t.Fatal(err)
	}
	var outer emulator.State
	awaitOuter(t, h, "the session's Kitty flags after the late reply", func(got emulator.State) bool {
		outer = got
		return got.Title == marker
	})
	// The reported support lets the outer terminal run the child's flags.
	if !outer.Alternate || outer.KittyKeyboardFlags != ghostty.KittyKeyDisambiguate {
		t.Fatalf("session outer alternate=%v flags=%d, want alternate flags=%d", outer.Alternate, outer.KittyKeyboardFlags, ghostty.KittyKeyDisambiguate)
	}
	if err := c.restore(); err != nil {
		t.Fatal(err)
	}
	// The next full-screen program enters the alternate screen the session left.
	// Its title follows the restoration in the same terminal stream.
	const next = "next-program"
	if _, err := h.slave.WriteString(ansi.SetModeAltScreenSaveCursor + ansi.SetWindowTitle(next)); err != nil {
		t.Fatal(err)
	}
	awaitOuter(t, h, "the next program's alternate screen", func(got emulator.State) bool {
		outer = got
		return got.Title == next
	})
	if !outer.Alternate || outer.KittyKeyboardFlags != found {
		t.Fatalf("next program's alternate=%v Kitty flags=%d, want alternate flags=%d", outer.Alternate, outer.KittyKeyboardFlags, found)
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
	if _, err := c.probe(ctx, nil, capabilityTimeout); err != nil {
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

// The outer terminal turns wheel steps into cursor keys under alternate scroll
// (DEC mode 1007). The session must leave that conversion on only while the
// child would convert: on its alternate screen, with alternate scroll set and
// no mouse tracking. The harness models Ghostty, which enables 1007 by default.
func TestSessionScopesOuterAlternateScrollToChild(t *testing.T) {
	type step struct {
		// line is typed into the child, which prints it as a printf format, so
		// marker is the last text the child writes for that step.
		line, marker string
		scroll       bool
	}
	for _, tt := range []struct {
		name  string
		entry bool
		// last leaves the outer mode opposite to its entry value, so the
		// restored value can only come from restoration.
		last step
	}{
		{"entry set", true, step{`\033[?1007h\033[?1049lprimary`, "primary", false}},
		{"entry reset", false, step{`\033[?1007hrescrolled`, "rescrolled", true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			steps := []step{
				{"", "start", false},
				// The child inherits the outer alternate scroll preference.
				{`\033[?1049halternate`, "alternate", tt.entry},
				{`\033[?1007hscrolling`, "scrolling", true},
				{`\033[?1000htracking`, "tracking", false},
				{`\033[?1000lreleased`, "released", true},
				{`\033[?1007lunscrolled`, "unscrolled", false},
				tt.last,
			}
			h := newHarness(t)
			if !tt.entry {
				h.mu.Lock()
				_, err := h.em.Write([]byte(ansi.ResetMode(alternateScrollMode)))
				h.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			child := exec.Command("sh", "-c", `stty -echo; printf start; while IFS= read -r line; do test "$line" = q && exit; printf "$line"; done`)
			done := startRun(ctx, New(child, struct{}{}).Terminal(h.slave, h.slave))
			for _, s := range steps {
				if s.line != "" {
					if err := h.writeReplies([]byte(s.line + "\r")); err != nil {
						t.Fatal(err)
					}
				}
				// The session flushes host input modes before it paints the
				// child output that changed them.
				awaitText(t, h, s.marker)
				h.mu.Lock()
				outer, err := h.em.State()
				h.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				if got := outer.Modes[ghostty.ModeAltScroll]; got != s.scroll {
					t.Errorf("outer alternate scroll after %q = %v, want %v", s.marker, got, s.scroll)
				}
			}
			if err := h.writeReplies([]byte("q\r")); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-done:
				if got.err != nil {
					t.Fatal(got.err)
				}
			case <-ctx.Done():
				t.Fatal("Run did not finish", ctx.Err())
			}
			awaitOuter(t, h, fmt.Sprintf("its entry alternate scroll %v", tt.entry), func(s emulator.State) bool {
				return !s.Alternate && s.Modes[ghostty.ModeAltScroll] == tt.entry
			})
		})
	}
}

// Every sample of the child's state decides the outer alternate scroll. The
// child read that switches screens updates it before anything is painted, as
// it does other host input modes, so a wheel step that arrives next already
// follows the child's screen. A full state refresh rebuilds every routing
// field from its own sample, the active screen included.
func TestChildStateSyncsOuterAlternateScroll(t *testing.T) {
	for _, tt := range []struct {
		name   string
		update func(t *testing.T, s *session[string], slave *os.File, chunk string)
	}{
		{"child read before painting", func(t *testing.T, s *session[string], slave *os.File, chunk string) {
			readRepaintChunk(t, s, slave, chunk)
		}},
		{"state refresh", func(t *testing.T, s *session[string], _ *os.File, chunk string) {
			if _, err := s.terminal.Write([]byte(chunk)); err != nil {
				t.Fatal(err)
			}
			if err := s.refresh(false); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, out, slave := newRepaintSession(t, ansi.ModeReset, nil)
			reportModes(t, s.console, map[ansi.DECMode]ansi.ModeSetting{alternateScrollMode: ansi.ModeReset})
			for _, step := range []struct{ chunk, want string }{
				{"\x1b[?1049h", ansi.SetMode(alternateScrollMode)},
				{"\x1b[?1049l", ansi.ResetMode(alternateScrollMode)},
			} {
				before := len(repaintOutput(t, out))
				tt.update(t, s, slave, step.chunk)
				if got := string(repaintOutput(t, out)[before:]); !strings.Contains(got, step.want) {
					t.Errorf("child output %q wrote %q to the outer terminal, want %q", step.chunk, got, step.want)
				}
			}
		})
	}
}

// The outer terminal reports the mouse in the child's single active tracking
// mode. Mouse tracking modes are mutually exclusive in xterm and Ghostty, so
// the console resets the previous mode before it sets the next one. Both the
// child read and a full state refresh carry the active mode.
func TestConsoleMirrorsActiveMouseTrackingMode(t *testing.T) {
	trackingModes := []ghostty.Mode{ghostty.ModeX10Mouse, ghostty.ModeNormalMouse, ghostty.ModeButtonMouse, ghostty.ModeAnyMouse}
	for _, update := range []struct {
		name string
		run  func(t *testing.T, s *session[string], slave *os.File, chunk string)
	}{
		{"child read", func(t *testing.T, s *session[string], slave *os.File, chunk string) {
			readRepaintChunk(t, s, slave, chunk)
		}},
		{"state refresh", func(t *testing.T, s *session[string], _ *os.File, chunk string) {
			if _, err := s.terminal.Write([]byte(chunk)); err != nil {
				t.Fatal(err)
			}
			if err := s.refresh(false); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		for _, tt := range []struct {
			name   string
			chunks []string
			want   ghostty.MouseTrackingMode
			// active is the outer tracking mode bit expected on with want.
			active ghostty.Mode
			// pixelsOnly reports 1016 permanently set before any cell size is
			// measured: tracking is withheld while the format follows the child.
			pixelsOnly bool
			// written and absent check the output of the last chunk.
			written, absent []string
		}{
			{
				name: "X10", chunks: []string{"\x1b[?9h"}, want: ghostty.MouseTrackingX10, active: ghostty.ModeX10Mouse,
				written: []string{"\x1b[?9h", "\x1b[?1006h"},
			},
			{
				name: "withheld until cells are measured", chunks: []string{"\x1b[?1003h", "\x1b[?9h\x1b[?1000h"}, want: ghostty.MouseTrackingNone,
				pixelsOnly: true, absent: []string{"\x1b[?9h", "\x1b[?1000h", "\x1b[?1003h"},
			},
			{
				name: "normal", chunks: []string{"\x1b[?1000h"}, want: ghostty.MouseTrackingNormal, active: ghostty.ModeNormalMouse,
				written: []string{"\x1b[?1000h", "\x1b[?1006h"},
			},
			{
				name: "reset after upgrade", chunks: []string{"\x1b[?1000h\x1b[?1002h\x1b[?1002l"}, want: ghostty.MouseTrackingNone,
				absent: []string{"\x1b[?1000h", "\x1b[?1002h", "\x1b[?1006h"},
			},
			{
				name: "reset after upgrade while tracking", chunks: []string{"\x1b[?1000h", "\x1b[?1002h\x1b[?1002l"}, want: ghostty.MouseTrackingNone,
				written: []string{"\x1b[?1000l", "\x1b[?1006l"}, absent: []string{"\x1b[?1002h"},
			},
			{
				name: "set and reset", chunks: []string{"\x1b[?1000h\x1b[?1000l"}, want: ghostty.MouseTrackingNone,
				absent: []string{"\x1b[?1000h", "\x1b[?1006h"},
			},
			{
				name: "later mode replaces earlier", chunks: []string{"\x1b[?1003h\x1b[?1000h"}, want: ghostty.MouseTrackingNormal, active: ghostty.ModeNormalMouse,
				written: []string{"\x1b[?1000h"}, absent: []string{"\x1b[?1003h"},
			},
			{
				name: "switch modes across reads", chunks: []string{"\x1b[?1003h", "\x1b[?1000h"}, want: ghostty.MouseTrackingNormal, active: ghostty.ModeNormalMouse,
				written: []string{"\x1b[?1003l", "\x1b[?1000h"},
			},
			{
				// Ghostty turns wheel steps into cursor keys while no tracking
				// mode is active, whatever mode bits remain set.
				name: "alternate scroll without active tracking", want: ghostty.MouseTrackingNone,
				chunks:  []string{"\x1b[?1049h\x1b[?1007h\x1b[?1000h\x1b[?1002h\x1b[?1002l"},
				written: []string{ansi.SetMode(alternateScrollMode)},
			},
		} {
			t.Run(update.name+"/"+tt.name, func(t *testing.T) {
				s, out, slave := newRepaintSession(t, ansi.ModeReset, nil)
				reportModes(t, s.console, map[ansi.DECMode]ansi.ModeSetting{
					9: ansi.ModeReset, 1000: ansi.ModeReset, 1002: ansi.ModeReset, 1003: ansi.ModeReset,
					1006: ansi.ModeReset, alternateScrollMode: ansi.ModeReset,
				})
				if tt.pixelsOnly {
					reportModes(t, s.console, map[ansi.DECMode]ansi.ModeSetting{1016: ansi.ModePermanentlySet})
				}
				outer, err := emulator.New(emulator.Options{Size: emulator.Size{Cols: 20, Rows: 6}})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(outer.Close)
				var last string
				fed := 0
				for _, chunk := range tt.chunks {
					update.run(t, s, slave, chunk)
					written := repaintOutput(t, out)
					if _, err := outer.Write(written[fed:]); err != nil {
						t.Fatal(err)
					}
					last, fed = string(written[fed:]), len(written)
				}
				for _, seq := range tt.written {
					if !strings.Contains(last, seq) {
						t.Errorf("outer terminal did not receive %q: %q", seq, last)
					}
				}
				for _, seq := range tt.absent {
					if strings.Contains(last, seq) {
						t.Errorf("outer terminal received %q: %q", seq, last)
					}
				}
				state, err := outer.State()
				if err != nil {
					t.Fatal(err)
				}
				if state.MouseTrackingMode != tt.want {
					t.Errorf("outer tracking mode = %v, want %v", state.MouseTrackingMode, tt.want)
				}
				for _, m := range trackingModes {
					if got := state.Modes[m]; got != (tt.want != ghostty.MouseTrackingNone && m == tt.active) {
						t.Errorf("outer mode %d = %v with tracking mode %v", m, got, tt.want)
					}
				}
				if got := state.Modes[ghostty.ModeSGRMouse]; got != (tt.want != ghostty.MouseTrackingNone || tt.pixelsOnly) {
					t.Errorf("outer SGR mouse = %v with tracking mode %v", got, tt.want)
				}
			})
		}
	}
}

// pressReport is the report em sends for a left press at pixel (25, 45), which
// lies in cell (3, 3) of a terminal with harness-sized cells. The native
// encoder applies em's effective tracking mode and report format.
func pressReport(t *testing.T, em *emulator.Terminal) string {
	t.Helper()
	event, err := ghostty.NewMouseEvent()
	if err != nil {
		t.Fatal(err)
	}
	defer event.Close()
	event.SetButton(ghostty.MouseButtonLeft)
	event.SetAction(ghostty.MouseActionPress)
	event.SetPosition(ghostty.MousePosition{X: 25, Y: 45})
	report, err := em.EncodeMouse(event, false)
	if err != nil {
		t.Fatal(err)
	}
	return string(report)
}

// A reset of any report format (1005, 1006, 1015, 1016) selects X10 on the
// outer terminal, so a child leaving pixel reports must keep the outer
// terminal on SGR cell reports, as the console records.
func TestConsoleKeepsOuterSGRAfterChildLeavesPixelMouse(t *testing.T) {
	s, out, slave := newRepaintSession(t, ansi.ModeReset, nil)
	c := s.console
	c.cellWidth, c.cellHeight = harnessCellWidth, harnessCellHeight
	reportModes(t, c, map[ansi.DECMode]ansi.ModeSetting{
		1000: ansi.ModeReset, 1005: ansi.ModeReset, 1006: ansi.ModeReset, 1015: ansi.ModeReset, 1016: ansi.ModeReset,
	})
	outer, err := emulator.New(emulator.Options{Size: emulator.Size{Cols: 20, Rows: 6, CellWidthPx: harnessCellWidth, CellHeightPx: harnessCellHeight}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(outer.Close)
	fed := 0
	for _, step := range []struct {
		chunk, report string
		applied       map[ansi.DECMode]bool
	}{
		{"\x1b[?1000;1016h", "\x1b[<0;25;45M", map[ansi.DECMode]bool{1005: false, 1006: false, 1015: false, 1016: true}},
		{"\x1b[?1016l", "\x1b[<0;3;3M", map[ansi.DECMode]bool{1005: false, 1006: true, 1015: false, 1016: false}},
	} {
		readRepaintChunk(t, s, slave, step.chunk)
		written := repaintOutput(t, out)
		if _, err := outer.Write(written[fed:]); err != nil {
			t.Fatal(err)
		}
		last := string(written[fed:])
		fed = len(written)
		if got := pressReport(t, outer); got != step.report {
			t.Errorf("after child %q (outer received %q), outer press report = %q, want %q", step.chunk, last, got, step.report)
		}
		for m, want := range step.applied {
			if c.applied[m] != want {
				t.Errorf("after child %q, applied[%d] = %v, want %v", step.chunk, m, c.applied[m], want)
			}
		}
	}
}

// Tracking (9, 1000-1003) and report format (1005, 1006, 1015, 1016) modes
// each form a group in which a reset after the set mode turns it off on the
// outer terminal. The entry tracking mode and format must stay active while the
// session enters and after it restores the terminal. Ghostty keeps the bits of
// modes a later set replaced and reports them set, so the last mode of each
// group reported set is the one left active.
func TestConsoleKeepsEntryMouseModesActive(t *testing.T) {
	for _, tt := range []struct {
		name, entry, report string
		tracking            ghostty.MouseTrackingMode
		// active are the modes applied as the effective tracking mode and format.
		active []ansi.DECMode
	}{
		{"one mode per group", "\x1b[?1000;1006h", "\x1b[<0;3;3M", ghostty.MouseTrackingNormal, []ansi.DECMode{1000, 1006}},
		{"replaced modes still reported", "\x1b[?1000;1002;1006;1016h", "\x1b[<0;25;45M", ghostty.MouseTrackingButton, []ansi.DECMode{1002, 1016}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.mu.Lock()
			if _, err := h.em.Write([]byte(tt.entry)); err != nil {
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
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := c.probe(ctx, nil, capabilityTimeout); err != nil {
				t.Fatal(err)
			}
			// check waits for a marker written after the console's output, then
			// compares the outer terminal's effective mouse modes with the entry.
			check := func(stage string, alternate bool) {
				t.Helper()
				if _, err := h.slave.WriteString(ansi.SetWindowTitle(stage)); err != nil {
					t.Fatal(err)
				}
				awaitOuter(t, h, "the marker written after "+stage, func(s emulator.State) bool {
					return s.Title == stage
				})
				h.mu.Lock()
				state, err := h.em.State()
				report := pressReport(t, h.em)
				h.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				if state.Alternate != alternate {
					t.Errorf("after %s, outer alternate screen = %v, want %v", stage, state.Alternate, alternate)
				}
				if state.MouseTrackingMode != tt.tracking {
					t.Errorf("after %s, outer tracking mode = %v, want %v", stage, state.MouseTrackingMode, tt.tracking)
				}
				if report != tt.report {
					t.Errorf("after %s, outer press report = %q, want %q", stage, report, tt.report)
				}
				for _, m := range []ansi.DECMode{9, 1000, 1001, 1002, 1003, 1005, 1006, 1015, 1016} {
					if want := slices.Contains(tt.active, m); c.applied[m] != want {
						t.Errorf("after %s, applied[%d] = %v, want %v", stage, m, c.applied[m], want)
					}
				}
			}
			if err := c.enter(); err != nil {
				t.Fatal(err)
			}
			check("enter", true)
			if err := c.restore(); err != nil {
				t.Fatal(err)
			}
			check("restore", false)
		})
	}
}

// The native outer terminal answers Primary Device Attributes (DA1) but not the
// modifyOtherKeys query. A terminal answers its queries in order, so the DA1
// reply the probe asks for last ends the probe long before its deadline, and
// the query the terminal ignores stays pending for a reply that may still come.
func TestProbeEndsAtPrimaryDeviceAttributesReply(t *testing.T) {
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
	// A deadline far past the fallback makes a probe that waits for the
	// unanswered query fail by a wide margin.
	const timeout = 10 * capabilityTimeout
	ctx, cancel := context.WithTimeout(context.Background(), 2*timeout)
	defer cancel()
	start := time.Now()
	if _, err := c.probe(ctx, nil, timeout); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed >= capabilityTimeout {
		t.Fatalf("probe took %v with an unanswered query, want it to end at the DA1 reply before %v", elapsed, capabilityTimeout)
	}
	if c.daPending {
		t.Fatal("probe ended without the outer terminal's DA1 reply")
	}
	if !c.modifyPending {
		t.Fatal("the unanswered modifyOtherKeys query was settled, so its late reply would reach the child")
	}
}

// Startup requires the alternate-screen report, so a terminal that sends it
// after its DA1 reply, out of order, still starts: the probe keeps waiting for
// that report until its deadline.
func TestProbeAwaitsAlternateScreenReportAfterPrimaryDeviceAttributes(t *testing.T) {
	const alternate = "\x1b[?1049;2$y"
	h := newHarness(t)
	h.report(ansi.ModeAltScreenSaveCursor, "")
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
	events := newEventDispatcher(eventBit(OuterInput), sessionCancel(t), func(e Event) {
		if strings.Contains(string(e.Bytes), "\x1b[?62;22c") {
			once.Do(func() { writeErr <- h.writeReplies([]byte(alternate)) })
		}
	})
	defer events.close()
	const timeout = 10 * capabilityTimeout
	ctx, cancel := context.WithTimeout(context.Background(), 2*timeout)
	defer cancel()
	start := time.Now()
	if _, err := c.probe(ctx, events, timeout); err != nil {
		t.Fatalf("probe with the alternate-screen report after DA1: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= timeout {
		t.Fatalf("probe took %v, want it to end at the alternate-screen report before its %v deadline", elapsed, timeout)
	}
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}
	if c.daPending || c.pending[ansi.ModeAltScreenSaveCursor] {
		t.Fatalf("probe ended with DA1 pending=%v and the alternate-screen report pending=%v", c.daPending, c.pending[ansi.ModeAltScreenSaveCursor])
	}
}

// Only the DA1 reply to the probe's own query is a reply. Once it arrived, a
// later report answers nothing the frame asked and stays input.
func TestConsoleConsumesOnlyPendingPrimaryDeviceAttributes(t *testing.T) {
	const reply = "\x1b[?62;22c"
	c := &console{daPending: true}
	if !c.consumeReply(decodePacket(t, reply)) {
		t.Fatalf("pending DA1 reply %q was not consumed", reply)
	}
	if c.daPending {
		t.Fatal("DA1 reply left its query pending")
	}
	if c.consumeReply(decodePacket(t, reply)) {
		t.Fatalf("DA1 report %q without a pending query was consumed", reply)
	}
}

// A terminal that answers DA1 only after the probe's deadline still answers
// the frame's query, so the reply never reaches the child as typed input.
func TestLatePrimaryDeviceAttributesReplyStaysWithFrame(t *testing.T) {
	h := newHarness(t)
	h.withholdPrimaryDeviceAttributes()
	s := newProbedSession(t, h)
	if !s.console.daPending {
		t.Fatal("probe settled the DA1 query the outer terminal did not answer")
	}
	if err := s.route(decodePacket(t, "\x1b[?62;22c")); err != nil {
		t.Fatal(err)
	}
	var routed []byte
	for _, p := range s.queue {
		routed = append(routed, p.bytes[p.offset:]...)
	}
	if len(routed) != 0 {
		t.Errorf("child received %q, want the late DA1 reply consumed", routed)
	}
	if s.console.daPending {
		t.Error("late DA1 reply left its query pending")
	}
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

func TestConsoleConsumesEveryCellSizeReply(t *testing.T) {
	const width, height = 7, 13
	for _, tt := range []struct {
		name, reply   string
		width, height uint32
	}{
		{"valid", "\x1b[6;20;10t", 10, 20},
		{"zero", "\x1b[6;0;0t", width, height},
		{"zero width", "\x1b[6;20;0t", width, height},
		{"zero height", "\x1b[6;0;10t", width, height},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// The probe's reply already cleared cellPending. A later report
			// answers another query the frame sent, never one from the child.
			c := &console{cellWidth: width, cellHeight: height}
			packets, err := input.New().Feed([]byte(tt.reply))
			if err != nil {
				t.Fatal(err)
			}
			if len(packets) != 1 {
				t.Fatalf("decoded %d packets from %q, want 1", len(packets), tt.reply)
			}
			if !c.consumeReply(packets[0]) {
				t.Fatalf("cell-size reply %q was not consumed", tt.reply)
			}
			if c.cellWidth != tt.width || c.cellHeight != tt.height {
				t.Fatalf("cell size after %q = %dx%d, want %dx%d", tt.reply, c.cellWidth, c.cellHeight, tt.width, tt.height)
			}
		})
	}
}

func TestProbeConsumesZeroCellSizeReply(t *testing.T) {
	const reply = "\x1b[6;0;0t"
	// The native outer terminal answers CSI 16 t with zero cell pixels.
	h := newCellHarness(t, 40, 12, 0, 0)
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
	var (
		mu       sync.Mutex
		received []byte
	)
	events := newEventDispatcher(eventBit(OuterInput), sessionCancel(t), func(e Event) {
		mu.Lock()
		received = append(received, e.Bytes...)
		mu.Unlock()
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	saved, err := c.probe(ctx, events, capabilityTimeout)
	events.close()
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(string(received), reply) {
		t.Fatalf("outer terminal did not answer the probe with %q: %q", reply, received)
	}
	// Run routes every saved packet to the child after entering the session.
	for _, p := range saved {
		if _, ok := p.Event.(uv.CellSizeEvent); ok {
			t.Fatalf("probe saved cell-size reply %q for the child", p.Raw)
		}
	}
	if c.cellWidth != 0 || c.cellHeight != 0 {
		t.Fatalf("zero reply measured cells %dx%d", c.cellWidth, c.cellHeight)
	}
}

// The session draws on the outer alternate screen and leaves it for the main
// screen the user was working in. A terminal that cannot switch ignores
// CSI ? 1049 h, so the session's first redraw would erase the main screen and
// leaving could not bring it back. Only a report of mode 1049 as reset (DECRPM
// Ps 2) shows an inactive alternate screen the terminal can switch to. Any
// other report, or none, fails startup before the frame writes the switch or
// starts the child.
func TestStartupRequiresSwitchableInactiveAlternateScreen(t *testing.T) {
	const mode, marker = ansi.ModeAltScreenSaveCursor, "startup-finished"
	for _, tt := range []struct {
		name, reply string
		accepted    bool
	}{
		{"not recognized", ansi.ReportMode(mode, ansi.ModeNotRecognized), false},
		{"set", ansi.ReportMode(mode, ansi.ModeSet), false},
		{"reset", ansi.ReportMode(mode, ansi.ModeReset), true},
		{"permanently set", ansi.ReportMode(mode, ansi.ModePermanentlySet), false},
		{"permanently reset", ansi.ReportMode(mode, ansi.ModePermanentlyReset), false},
		{"not reported", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var want error
			if !tt.accepted {
				want = errAlternateScreen
			}
			h := newHarness(t)
			h.report(mode, tt.reply)
			h.transcribe()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			fd, device, _, err := inspectConsole(h.slave, h.slave)
			if err != nil {
				t.Fatal(err)
			}
			c, err := acquireConsole(h.slave, h.slave, fd, device)
			if err != nil {
				t.Fatal(err)
			}
			_, probeErr := c.probe(ctx, nil, capabilityTimeout)
			if err := c.restore(); err != nil {
				t.Fatal(err)
			}
			if !errors.Is(probeErr, want) {
				t.Errorf("probe with mode 1049 reply %q returned %v, want %v", tt.reply, probeErr, want)
			}
			child := exec.Command("true")
			result, err := New(child, struct{}{}).Terminal(h.slave, h.slave).Run(ctx)
			if !errors.Is(err, want) {
				t.Errorf("Run with mode 1049 reply %q returned %v, want %v", tt.reply, err, want)
			}
			if started := child.Process != nil; started != tt.accepted {
				t.Errorf("Run with mode 1049 reply %q started the child = %v, want %v", tt.reply, started, tt.accepted)
			}
			if tt.accepted && (result.ProcessState == nil || !result.ProcessState.Success()) {
				t.Errorf("Run with mode 1049 reply %q returned child state %v, want success", tt.reply, result.ProcessState)
			}
			// The marker follows everything Run wrote, so the transcript is
			// complete once the outer terminal shows it.
			if _, err := h.slave.WriteString(ansi.SetWindowTitle(marker)); err != nil {
				t.Fatal(err)
			}
			awaitOuter(t, h, "the marker written after Run", func(s emulator.State) bool {
				return s.Title == marker
			})
			if entered := strings.Contains(h.transcribed(), ansi.SetModeAltScreenSaveCursor); entered != tt.accepted {
				t.Errorf("frame wrote %q with mode 1049 reply %q = %v, want %v", ansi.SetModeAltScreenSaveCursor, tt.reply, entered, tt.accepted)
			}
		})
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
		pixels          bool
		outer, routed   string
		// unmeasured cases start before the cell-size reply arrives. pixelless
		// cases resize to a winsize without pixels before outer is routed. late
		// sequences must reach the outer terminal in order after the child
		// chunk. withheld is the error a Routed event reports when the session
		// withholds outer as an event the child's protocol cannot represent.
		unmeasured, pixelless bool
		late                  []string
		withheld              string
	}{
		{
			name: "cursor keys permanently reset", reports: modes{1: ansi.ModePermanentlyReset},
			child: "\x1b[?1h", absent: []string{"\x1b[?1h"}, applied: map[ansi.DECMode]bool{1: false},
			// The terminal keeps its own cursor-key form, which reaches the child as sent.
			outer: "\x1b[A", routed: "\x1b[A",
		},
		{
			name: "cursor keys permanently set", reports: modes{1: ansi.ModePermanentlySet},
			child: "\x1b[?1l", absent: []string{"\x1b[?1l"}, applied: map[ansi.DECMode]bool{1: true},
			outer: "\x1bOA", routed: "\x1bOA",
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
			// A cell report cannot supply the pixel precision the child asked
			// for, so the child receives nothing and the session continues.
			outer: "\x1b[<0;7;5M", withheld: "child pixel mouse requires pixel input from the outer terminal",
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
			var withheld []error
			s.events = newEventDispatcher(eventBit(Routed), sessionCancel(t), func(ev Event) {
				if ev.Origin == "unroutable-input" {
					withheld = append(withheld, ev.Error)
				}
			})
			t.Cleanup(s.events.close)
			for _, p := range packets {
				if err := s.route(p); err != nil {
					t.Fatalf("routing %q failed: %v", tt.outer, err)
				}
			}
			s.events.close()
			switch {
			case tt.withheld == "" && len(withheld) != 0:
				t.Errorf("routing %q withheld input: %v", tt.outer, withheld)
			case tt.withheld != "" && (len(withheld) != 1 || !strings.Contains(withheld[0].Error(), tt.withheld)):
				t.Errorf("routing %q withheld input with %v, want one error %q", tt.outer, withheld, tt.withheld)
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
			if host := s.router.host; host.MousePixels != tt.pixels {
				t.Errorf("host profile pixels=%v, want %v", host.MousePixels, tt.pixels)
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
	if _, err := c.probe(ctx, nil, capabilityTimeout); err != nil {
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
	// A child in modifyOtherKeys mode 2 gets the reported mode on the outer
	// terminal, whose native state agrees with the report decoded above.
	if err := c.syncInput(NativeState{ModifyOtherKeys2: true}); err != nil {
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
		t.Fatal("outer modifyOtherKeys did not match child mode 2")
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
	if _, err := c.probe(ctx, events, capabilityTimeout); err != nil {
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

// Each child read mirrors the child's keyboard and focus modes on the outer
// terminal from the routing state that read refreshes, in both directions.
func TestChildReadMirrorsInputModes(t *testing.T) {
	for _, m := range []ansi.DECMode{1, 66, 67, 1004, 1035, 1036, 1039} {
		t.Run(fmt.Sprint(m), func(t *testing.T) {
			s, out, slave := newRepaintSession(t, ansi.ModeReset, nil)
			s.console.entry[m] = ansi.ModeReset
			for _, on := range []bool{true, false} {
				seq := ansi.ResetMode(m)
				if on {
					seq = ansi.SetMode(m)
				}
				readRepaintChunk(t, s, slave, seq)
				if s.console.applied[m] != on {
					t.Fatalf("after the child wrote %q, outer mode %d applied = %v", seq, m, s.console.applied[m])
				}
			}
			if output := repaintOutput(t, out); !bytes.Contains(output, []byte(ansi.SetMode(m)+ansi.ResetMode(m))) {
				t.Errorf("outer terminal received %q, want mode %d set and then reset", output, m)
			}
		})
	}
}
