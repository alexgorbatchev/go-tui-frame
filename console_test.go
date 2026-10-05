package frame

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexgorbatchev/go-tui-frame/internal/emulator"
	"github.com/alexgorbatchev/go-tui-frame/internal/input"
	"github.com/charmbracelet/x/ansi"
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
