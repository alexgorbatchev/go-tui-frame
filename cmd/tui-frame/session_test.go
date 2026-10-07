package main

import (
	"context"
	"errors"
	"fmt"
	"image/color"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	frame "github.com/alexgorbatchev/go-tui-frame/v2"
	"github.com/alexgorbatchev/go-tui-frame/v2/internal/emulator"
	charmterm "github.com/charmbracelet/x/term"
	"github.com/creack/pty"
	ghostty "go.mitchellh.com/libghostty"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const sessionTimeout = 10 * time.Second

func TestCLIInheritanceChild(t *testing.T) {
	path := os.Getenv("FRAME_CLI_INHERITANCE_FILE")
	if path == "" {
		return
	}
	state, err := charmterm.GetState(os.Stdin.Fd())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte{state.Cc[unix.VERASE]}, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCLIInheritanceFlagControlsRealPTYSettings(t *testing.T) {
	binary := buildExample(t)
	for _, tt := range []struct {
		name    string
		flags   []string
		inherit bool
	}{
		{name: "default", inherit: true},
		{name: "disabled", flags: []string{"--no-terminal-inheritance"}},
		{name: "showcase with inheritance disabled", flags: []string{"--showcase", "--no-terminal-inheritance"}},
		{name: "explicitly enabled", flags: []string{"--no-terminal-inheritance=false"}, inherit: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, slave := demoTTY(t)
			state, err := charmterm.GetState(slave.Fd())
			if err != nil {
				t.Fatal(err)
			}
			state.Cc[unix.VERASE] = '#'
			if err := charmterm.SetState(slave.Fd(), state); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(projectTempDir(t), "erase-byte")
			ctx, cancel := context.WithTimeout(context.Background(), sessionTimeout)
			defer cancel()
			args := append(append([]string{}, tt.flags...), "--", os.Args[0], "-test.run=^TestCLIInheritanceChild$")
			cmd := exec.CommandContext(ctx, binary, args...)
			cmd.Env = append(os.Environ(), "FRAME_CLI_INHERITANCE_FILE="+file)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
			if err := cmd.Run(); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(file)
			if err != nil || len(got) != 1 || (got[0] == '#') != tt.inherit {
				t.Fatalf("child erase byte=%q err=%v; inheritance=%v", got, err, tt.inherit)
			}
		})
	}
}

func demoTTY(t *testing.T) (*os.File, *os.File) {
	return demoTTYObserved(t, nil)
}

func demoTTYObserved(t *testing.T, observed func(emulator.State)) (*os.File, *os.File) {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := slave.Close(); err != nil {
			t.Errorf("close slave: %v", err)
		}
	})
	// Close-on-exec keeps subprocesses from holding the outer terminal open, so
	// closing this master hangs up the PTY as closing a terminal tab does.
	fd, err := unix.FcntlInt(master.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := master.Close(); err != nil {
		t.Fatal(err)
	}
	if err := syscall.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	master = os.NewFile(uintptr(fd), "demo-pty-master")
	t.Cleanup(func() {
		if err := master.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Errorf("close master: %v", err)
		}
	})
	if err := pty.Setsize(slave, &pty.Winsize{Rows: 20, Cols: 100}); err != nil {
		t.Fatal(err)
	}
	// A nonblocking descriptor adopted by NewFile is pollable, so its deadline
	// and Close can stop the output drain without leaking a goroutine.
	if err := master.SetReadDeadline(time.Now().Add(sessionTimeout)); err != nil {
		t.Fatal(err)
	}
	outer, err := emulator.New(emulator.Options{Size: emulator.Size{Cols: 100, Rows: 20, CellWidthPx: 8, CellHeightPx: 16}})
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		defer outer.Close()
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				if _, err := outer.Write(buf[:n]); err != nil {
					t.Errorf("parse outer terminal output: %v", err)
					return
				}
				for _, effect := range outer.Effects() {
					if effect.Kind == emulator.Reply {
						if _, err := master.Write(effect.Bytes); err != nil {
							t.Errorf("write native outer terminal reply: %v", err)
							return
						}
					}
				}
				if observed != nil {
					state, err := outer.State()
					if err != nil {
						t.Errorf("read native outer state: %v", err)
						return
					}
					observed(state)
				}
			}
			if err != nil {
				if !errors.Is(err, os.ErrDeadlineExceeded) && !errors.Is(err, os.ErrClosed) && !errors.Is(err, syscall.EIO) {
					t.Errorf("drain outer PTY: %v", err)
				}
				return
			}
		}
	}()
	t.Cleanup(func() {
		if err := master.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Errorf("stop PTY drain: %v", err)
		}
		select {
		case <-drained:
		case <-time.After(sessionTimeout):
			t.Error("outer PTY drain did not stop")
		}
	})
	return master, slave
}

type regionPaint struct {
	region string
	data   UIData
	term   frame.Snapshot
	bg     color.Color
}

func waitPaint(t *testing.T, paints <-chan regionPaint, match func(regionPaint) bool) regionPaint {
	t.Helper()
	deadline := time.NewTimer(sessionTimeout)
	defer deadline.Stop()
	for {
		select {
		case paint := <-paints:
			if match(paint) {
				return paint
			}
		case <-deadline.C:
			t.Fatal("expected region paint did not arrive")
		}
	}
}

// demoOutcome is what a demo session's Run returned.
type demoOutcome struct {
	result frame.Result
	err    error
}

// startDemo starts run in the background and delivers its outcome. When the
// test ends, cancel stops the session and the test waits for Run to return.
func startDemo(t *testing.T, cancel context.CancelFunc, run func() (frame.Result, error)) <-chan demoOutcome {
	t.Helper()
	done := make(chan demoOutcome, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		result, err := run()
		done <- demoOutcome{result, err}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(sessionTimeout):
			t.Error("demo session did not stop")
		}
	})
	return done
}

func TestDemoControlsRepaintAndResizeRealChild(t *testing.T) {
	// corners receives the outer cell where the child border's corner sits.
	corners := make(chan string, 128)
	master, slave := demoTTYObserved(t, func(s emulator.State) {
		corner := ""
		if i := headerRows * s.Size.Cols; i < len(s.Cells) {
			corner = s.Cells[i].Content
		}
		select {
		case corners <- corner:
		default:
		}
	})
	before, err := term.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	inputFile := filepath.Join(projectTempDir(t), "child-input")
	sizeFile := filepath.Join(projectTempDir(t), "child-sizes")
	child := exec.Command("sh", "-c", `stty -echo; sizes=0; printf '\033]2;demo-child\007'; while IFS= read -r line; do if test "$line" = size; then stty size >> "$1"; sizes=$((sizes+1)); printf '\033]2;sized-%d\007' "$sizes"; else printf '%s' "$line" > "$2"; break; fi; done`, "sh", sizeFile, inputFile)
	ctx, cancel := context.WithTimeout(context.Background(), sessionTimeout)
	defer cancel()
	ctx, quit := context.WithCancelCause(ctx)
	defer quit(nil)
	app := newDemoSession(child, UIData{}, quit).frame.Terminal(slave, slave)
	paints := make(chan regionPaint, 64)
	app.Header(headerRows, func(ctx frame.DrawContext[UIData]) {
		drawHeader(ctx)
		paints <- regionPaint{region: "header", data: ctx.Data, term: ctx.Term, bg: ctx.View.CellAt(0, 0).Style.Bg}
	}).Footer(footerRows, func(ctx frame.DrawContext[UIData]) {
		drawFooter(ctx)
		paints <- regionPaint{region: "footer", data: ctx.Data, term: ctx.Term}
	})
	done := startDemo(t, cancel, func() (frame.Result, error) { return app.Run(ctx) })
	ready := waitPaint(t, paints, func(p regionPaint) bool {
		return p.term.Terminal.Title == "demo-child" && p.term.Child.PID > 0
	})
	if ready.term.Viewport != (frame.Size{Cols: 98, Rows: 13}) || ready.term.Child.Executable == "" {
		t.Fatalf("metadata does not describe the real child viewport: %+v", ready.term)
	}
	querySize := func(expected string) {
		t.Helper()
		if _, err := io.WriteString(master, "size\n"); err != nil {
			t.Fatal(err)
		}
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			bytes, err := os.ReadFile(sizeFile)
			if err == nil && string(bytes) == expected {
				return
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				t.Fatalf("actual child PTY size output %q, error %v; want %q", bytes, err, expected)
			}
		}
	}
	querySize("13 98\n")
	waitBorder := func(enabled bool) {
		t.Helper()
		for {
			select {
			case corner := <-corners:
				if (corner == lipgloss.RoundedBorder().TopLeft) == enabled {
					return
				}
			case <-ctx.Done():
				t.Fatalf("actual outer terminal border did not become %v", enabled)
			}
		}
	}
	waitBorder(true)
	for _, change := range []struct {
		key        string
		demo, bg   int
		border     bool
		viewport   frame.Size
		colour     string
		sizeOutput string
	}{
		{"\x021", 1, 0, true, frame.Size{Cols: 98, Rows: 13}, red, ""},
		{"\x021", 2, 0, true, frame.Size{Cols: 98, Rows: 13}, red, ""},
		{"\x021", 0, 0, true, frame.Size{Cols: 98, Rows: 13}, red, ""},
		{"\x022", 0, 1, true, frame.Size{Cols: 98, Rows: 13}, navy, ""},
		{"\x022", 0, 2, true, frame.Size{Cols: 98, Rows: 13}, teal, ""},
		{"\x022", 0, 0, true, frame.Size{Cols: 98, Rows: 13}, red, ""},
		{"\x023", 0, 0, false, frame.Size{Cols: 100, Rows: 15}, red, "13 98\n15 100\n"},
		{"\x023", 0, 0, true, frame.Size{Cols: 98, Rows: 13}, red, "13 98\n15 100\n13 98\n"},
	} {
		if change.sizeOutput != "" {
			// Discard old renders before checking the next actual border state.
			for len(corners) > 0 {
				<-corners
			}
		}
		if _, err := io.WriteString(master, change.key); err != nil {
			t.Fatal(err)
		}
		seen := make(map[string]bool)
		for len(seen) < 2 {
			p := waitPaint(t, paints, func(p regionPaint) bool {
				return p.data.Demo == change.demo && p.data.Background == change.bg && p.data.Border == change.border && p.term.Viewport == change.viewport
			})
			if p.region == "header" && (p.bg == nil || !sameColor(p.bg, lipgloss.Color(change.colour))) {
				t.Fatalf("native header background = %#v, want %s", p.bg, change.colour)
			}
			seen[p.region] = true
		}
		if change.sizeOutput != "" {
			querySize(change.sizeOutput)
			waitBorder(change.border)
		}
	}
	// Another key after the prefix ends it and reaches neither the demo nor the
	// child, whose next line is still "size". Ordinary digits and function keys
	// remain child input.
	if _, err := io.WriteString(master, "\x02x"); err != nil {
		t.Fatal(err)
	}
	querySize("13 98\n15 100\n13 98\n13 98\n")
	discarded := waitPaint(t, paints, func(p regionPaint) bool { return p.term.Terminal.Title == "sized-4" })
	if discarded.data.Demo != 0 || discarded.data.Background != 0 || !discarded.data.Border {
		t.Fatalf("the discarded key changed the demo: %+v", discarded.data)
	}
	if _, err := io.WriteString(master, "123\x1b[15~\x1b[17~ hello child\n"); err != nil {
		t.Fatal(err)
	}
	if outcome := <-done; outcome.err != nil || outcome.result.ProcessState == nil || outcome.result.ProcessState.ExitCode() != 0 {
		t.Fatalf("child did not exit successfully: %+v, %v", outcome.result, outcome.err)
	}
	got, err := os.ReadFile(inputFile)
	if err != nil || string(got) != "123\x1b[15~\x1b[17~ hello child" {
		t.Fatalf("captured keys reached child or ordinary input changed: %q, %v", got, err)
	}
	after, err := term.GetState(int(slave.Fd()))
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("outer terminal state was not restored: %v", err)
	}
}

func TestDemoQuitPreservesCauseAndReapsChild(t *testing.T) {
	for _, tt := range []struct {
		name  string
		input string
		kitty bool
	}{
		{"legacy", "\x11", false},
		// A Kitty terminal sets an enabled lock's bit on the Ctrl+Q it reports.
		{"kitty caps lock", "\x1b[113;69u", true},
		{"kitty num lock", "\x1b[113;133u", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			negotiated := make(chan struct{}, 1)
			master, slave := demoTTYObserved(t, func(s emulator.State) {
				if s.KittyKeyboardFlags&ghostty.KittyKeyDisambiguate != 0 {
					select {
					case negotiated <- struct{}{}:
					default:
					}
				}
			})
			inputFile := filepath.Join(projectTempDir(t), "child-input")
			if err := os.WriteFile(inputFile, nil, 0600); err != nil {
				t.Fatal(err)
			}
			// The raw child records the first byte it reads, so a Ctrl+Q that
			// capture passes on ends the session as an ordinary child exit. A
			// Kitty child requests disambiguation, which the outer terminal mirrors.
			modes := ""
			if tt.kitty {
				modes = `\033[>1u`
			}
			child := exec.Command("sh", "-c", `stty raw -echo; printf "$2\033]2;raw-child\007"; dd bs=1 count=1 of="$1" 2>/dev/null`, "sh", inputFile, modes)
			ctx, cancel := context.WithTimeout(context.Background(), sessionTimeout)
			defer cancel()
			ctx, quit := context.WithCancelCause(ctx)
			defer quit(nil)
			app := newDemoSession(child, UIData{}, quit).frame.Terminal(slave, slave)
			ready := make(chan struct{}, 1)
			app.Header(headerRows, func(ctx frame.DrawContext[UIData]) {
				drawHeader(ctx)
				if ctx.Term.Child.PID > 0 && ctx.Term.Terminal.Title == "raw-child" {
					select {
					case ready <- struct{}{}:
					default:
					}
				}
			})
			done := startDemo(t, cancel, func() (frame.Result, error) { return app.Run(ctx) })
			select {
			case <-ready:
			case <-ctx.Done():
				t.Fatal("child never painted its PID in raw mode")
			}
			if tt.kitty {
				select {
				case <-negotiated:
				case <-ctx.Done():
					t.Fatal("outer terminal did not mirror the child's Kitty disambiguation")
				}
			}
			if _, err := io.WriteString(master, tt.input); err != nil {
				t.Fatal(err)
			}
			got := <-done
			read, readErr := os.ReadFile(inputFile)
			if !errors.Is(got.err, errDemoQuit) || readErr != nil || len(read) != 0 {
				t.Fatalf("input %q: err=%v, child read %q (%v); want a quit before the child reads anything",
					tt.input, got.err, read, readErr)
			}
			if state := got.result.ProcessState; state == nil || !state.Exited() && state.ExitCode() != -1 {
				t.Fatalf("quit left the child unreaped: %+v", got.result)
			}
			if got.result.DrainError != nil || got.result.CleanupError != nil {
				t.Fatalf("quit failed cleanup: %+v", got.result)
			}
		})
	}
}

// keyStroke is one key event a terminal reports. text must be a string
// literal: the native event borrows its storage until the event is encoded.
type keyStroke struct {
	key       ghostty.Key
	mods      ghostty.Mods
	text      string
	unshifted rune
	action    ghostty.KeyAction
}

// reportKeys encodes strokes as a terminal running the Kitty keyboard flags
// reports them, using the native key encoder. A legacy terminal (flags 0)
// reports no releases.
func reportKeys(t *testing.T, flags ghostty.KittyKeyFlags, strokes ...keyStroke) string {
	t.Helper()
	encoder, err := ghostty.NewKeyEncoder()
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	encoder.SetOptKittyFlags(flags)
	event, err := ghostty.NewKeyEvent()
	if err != nil {
		t.Fatal(err)
	}
	defer event.Close()
	var report []byte
	for _, s := range strokes {
		event.SetKey(s.key)
		event.SetMods(s.mods)
		event.SetUTF8(s.text)
		event.SetUnshiftedCodepoint(s.unshifted)
		event.SetAction(s.action)
		encoded, err := encoder.Encode(event)
		if err != nil {
			t.Fatal(err)
		}
		report = append(report, encoded...)
	}
	return string(report)
}

// The demo's prefix keys pass through a real session to its capture handler,
// as a legacy terminal reports them and with Kitty disambiguation and release
// events, which the outer terminal runs because the child requests them. A
// release or repeat between Ctrl+B and the digit leaves the prefix in place, a
// second Ctrl+B reaches the child as the terminal sent it, another key after
// the prefix is discarded, and Ctrl+Q after the prefix quits.
func TestDemoPrefixKeysThroughRealSession(t *testing.T) {
	const press, repeat, release = ghostty.KeyActionPress, ghostty.KeyActionRepeat, ghostty.KeyActionRelease
	ctrlB := func(action ghostty.KeyAction) keyStroke {
		return keyStroke{ghostty.KeyB, ghostty.ModCtrl, "b", 'b', action}
	}
	ctrlQ := func(action ghostty.KeyAction) keyStroke {
		return keyStroke{ghostty.KeyQ, ghostty.ModCtrl, "q", 'q', action}
	}
	digit := func(key ghostty.Key, text string, action ghostty.KeyAction) keyStroke {
		return keyStroke{key, 0, text, rune(text[0]), action}
	}
	one := func(action ghostty.KeyAction) keyStroke { return digit(ghostty.KeyDigit1, "1", action) }
	two := func(action ghostty.KeyAction) keyStroke { return digit(ghostty.KeyDigit2, "2", action) }
	three := func(action ghostty.KeyAction) keyStroke { return digit(ghostty.KeyDigit3, "3", action) }
	x := func(action ghostty.KeyAction) keyStroke { return keyStroke{ghostty.KeyX, 0, "x", 'x', action} }
	for _, form := range []struct {
		name, modes string
		flags       ghostty.KittyKeyFlags
	}{
		{"legacy", "", 0},
		{"Kitty with release events", `\033[>3u`, ghostty.KittyKeyDisambiguate | ghostty.KittyKeyReportEvents},
	} {
		t.Run(form.name, func(t *testing.T) {
			master, slave := demoTTY(t)
			inputFile := filepath.Join(projectTempDir(t), "child-input")
			if err := os.WriteFile(inputFile, nil, 0600); err != nil {
				t.Fatal(err)
			}
			// The raw child records every byte it reads.
			child := exec.Command("sh", "-c", `stty raw -echo; printf "$2\033]2;prefix-child\007"; exec dd bs=1 of="$1" 2>/dev/null`, "sh", inputFile, form.modes)
			ctx, cancel := context.WithTimeout(context.Background(), sessionTimeout)
			defer cancel()
			ctx, quit := context.WithCancelCause(ctx)
			defer quit(nil)
			app := newDemoSession(child, UIData{}, quit).frame.Terminal(slave, slave)
			paints := make(chan regionPaint, 64)
			app.Header(headerRows, func(ctx frame.DrawContext[UIData]) {
				drawHeader(ctx)
				paints <- regionPaint{region: "header", data: ctx.Data, term: ctx.Term}
			})
			done := startDemo(t, cancel, func() (frame.Result, error) { return app.Run(ctx) })
			waitPaint(t, paints, func(p regionPaint) bool { return p.term.Terminal.Title == "prefix-child" && p.term.Child.PID > 0 })
			send := func(strokes ...keyStroke) {
				t.Helper()
				if _, err := io.WriteString(master, reportKeys(t, form.flags, strokes...)); err != nil {
					t.Fatal(err)
				}
			}
			awaitDemo := func(want UIData) {
				t.Helper()
				waitPaint(t, paints, func(p regionPaint) bool { return p.data == want })
			}
			awaitChild := func(want string) {
				t.Helper()
				for {
					got, err := os.ReadFile(inputFile)
					if err == nil && string(got) == want {
						return
					}
					select {
					case <-ctx.Done():
						t.Fatalf("child read %q, %v; want %q", got, err, want)
					case <-time.After(time.Millisecond):
					}
				}
			}
			send(ctrlB(press), ctrlB(release), one(press), one(release))
			awaitDemo(UIData{Demo: 1, Border: true})
			held := []keyStroke{ctrlB(press)}
			if form.flags != 0 {
				// A legacy terminal repeats a held key as presses.
				held = append(held, ctrlB(repeat), ctrlB(repeat))
			}
			send(append(held, ctrlB(release), two(press), two(release))...)
			awaitDemo(UIData{Demo: 1, Background: 1, Border: true})
			send(ctrlB(press), ctrlB(release), three(press), three(release))
			awaitDemo(UIData{Demo: 1, Background: 1})
			forwarded := reportKeys(t, form.flags, ctrlB(press), ctrlB(release))
			send(ctrlB(press), ctrlB(release), ctrlB(press), ctrlB(release))
			awaitChild(forwarded)
			typed := reportKeys(t, form.flags, one(press), one(release))
			send(ctrlB(press), ctrlB(release), x(press), x(release), one(press), one(release))
			awaitChild(forwarded + typed)
			// Ctrl+Q has no prefix binding and falls through to quit.
			send(ctrlB(press), ctrlB(release), ctrlQ(press), ctrlQ(release))
			var got demoOutcome
			select {
			case got = <-done:
			case <-time.After(sessionTimeout):
				// Ctrl+Q cancels ctx, so only Run's return shows it took effect.
				t.Fatal("Ctrl+Q did not end the session")
			}
			read, err := os.ReadFile(inputFile)
			if !errors.Is(got.err, errDemoQuit) || err != nil || string(read) != forwarded+typed {
				t.Fatalf("Run = %v; child read %q, %v; want the quit after %q", got.err, read, err, forwarded+typed)
			}
		})
	}
}

func TestSessionFailureRemovesOnlyTheQuitRequest(t *testing.T) {
	// These mirror the library's errors: the loop joins the context's cause with
	// the SIGTERM and SIGCONT deliveries, then a render, deadline, or read
	// failure, and Run joins the cleanup outcome on top.
	signalErr := fmt.Errorf("signal child process group: %w", syscall.EPERM)
	renderErr := fmt.Errorf("render outer terminal: %w", syscall.EIO)
	// Run builds CleanupError with errors.Join and joins that same value into
	// its error, so the filter must keep the node itself.
	cleanupErr := errors.Join(nil, fmt.Errorf("restore terminal modes: %w", syscall.EIO))
	interrupted := &signalCause{signal: syscall.SIGINT}
	tests := []struct {
		name string
		err  error
		// want is the reported message; empty means the run succeeded.
		want string
		kept []error
	}{
		{"no error", nil, "", nil},
		{"quit only", errors.Join(errors.Join(errors.Join(errDemoQuit, nil, nil), nil), nil), "", nil},
		{
			"quit with a final render failure",
			errors.Join(errors.Join(errors.Join(errDemoQuit, nil, nil), renderErr), nil),
			"run child frame: " + renderErr.Error(),
			[]error{renderErr, syscall.EIO},
		},
		{
			"quit with later failures",
			errors.Join(errors.Join(errors.Join(errDemoQuit, signalErr), renderErr), cleanupErr),
			"run child frame: " + signalErr.Error() + "\n" + renderErr.Error() + "\n" + cleanupErr.Error(),
			[]error{signalErr, renderErr, cleanupErr, syscall.EPERM, syscall.EIO},
		},
		{
			"signal cancellation",
			errors.Join(errors.Join(errors.Join(interrupted, nil, nil), nil), cleanupErr),
			"run child frame: interrupt signal received\n" + cleanupErr.Error(),
			[]error{interrupted, cleanupErr},
		},
		{"context cancellation", context.Canceled, "run child frame: context canceled", []error{context.Canceled}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sessionFailure(tt.err)
			if tt.want == "" {
				if got != nil {
					t.Fatalf("sessionFailure = %q, want nil", got)
				}
				return
			}
			if got == nil || got.Error() != tt.want {
				t.Fatalf("sessionFailure = %v, want %q", got, tt.want)
			}
			if errors.Is(got, errDemoQuit) {
				t.Fatalf("sessionFailure kept the quit request: %q", got)
			}
			for _, kept := range tt.kept {
				if !errors.Is(got, kept) {
					t.Errorf("sessionFailure dropped %v: %q", kept, got)
				}
			}
		})
	}
}

func TestAgentDemoStartsPlainAndCanEnableChildBorder(t *testing.T) {
	master, slave := demoTTY(t)
	ctx, cancel := context.WithTimeout(context.Background(), sessionTimeout)
	defer cancel()
	ctx, quit := context.WithCancelCause(ctx)
	defer quit(nil)
	child := exec.Command("sh", "-c", "sleep 30")
	app := newDemoSession(child, UIData{Agent: true}, quit).frame.Terminal(slave, slave)
	paints := make(chan regionPaint, 32)
	app.Header(headerRows, func(ctx frame.DrawContext[UIData]) {
		drawHeader(ctx)
		paints <- regionPaint{data: ctx.Data, term: ctx.Term, bg: ctx.View.CellAt(0, 0).Style.Bg}
	})
	startDemo(t, cancel, func() (frame.Result, error) { return app.Run(ctx) })
	initial := waitPaint(t, paints, func(p regionPaint) bool { return p.term.Child.PID > 0 })
	if initial.data.Border || initial.term.Viewport != (frame.Size{Cols: 100, Rows: 15}) || initial.bg != nil {
		t.Fatalf("agent defaults are not plain and borderless: %+v", initial)
	}
	if _, err := io.WriteString(master, "\x023"); err != nil {
		t.Fatal(err)
	}
	enabled := waitPaint(t, paints, func(p regionPaint) bool { return p.data.Border })
	if enabled.term.Viewport != (frame.Size{Cols: 98, Rows: 13}) || enabled.bg != nil {
		t.Fatalf("agent border toggle lost plain styling or geometry: %+v", enabled)
	}
}
