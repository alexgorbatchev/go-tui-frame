package frame

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexgorbatchev/go-tui-frame/internal/input"
	"github.com/charmbracelet/x/ansi"
	ghostty "go.mitchellh.com/libghostty"
	"golang.org/x/sys/unix"
)

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
	if err := c.restore(); err != nil {
		t.Fatal(err)
	}
	if !disabled {
		t.Fatal("outer modifyOtherKeys did not match child legacy mode")
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
	events := newEventDispatcher(func(e Event) {
		if e.Kind == OuterInput && strings.Contains(string(e.Bytes), "\x1b[?1049;2$y") {
			once.Do(func() { _, err := unix.Write(h.fd, []byte("\x1b[200~before")); writeErr <- err })
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
