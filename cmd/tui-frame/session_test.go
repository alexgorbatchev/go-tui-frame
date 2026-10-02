package main

import (
	"context"
	"errors"
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
	frame "github.com/alexgorbatchev/go-tui-frame"
	"github.com/alexgorbatchev/go-tui-frame/internal/emulator"
	"github.com/creack/pty"
	ghostty "go.mitchellh.com/libghostty"
	"golang.org/x/term"
)

const sessionTimeout = 10 * time.Second

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
	fd, err := syscall.Dup(int(master.Fd()))
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

func TestDemoControlsRepaintAndResizeRealChild(t *testing.T) {
	type outerObservation struct {
		flags  ghostty.KittyKeyFlags
		corner string
	}
	states := make(chan outerObservation, 128)
	master, slave := demoTTYObserved(t, func(s emulator.State) {
		observation := outerObservation{flags: s.KittyKeyboardFlags}
		if i := headerRows * s.Size.Cols; i < len(s.Cells) {
			observation.corner = s.Cells[i].Content
		}
		select {
		case states <- observation:
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
	app := newDemoFrame(child, UIData{}, quit).Terminal(slave, slave)
	paints := make(chan regionPaint, 64)
	app.Header(headerRows, func(ctx frame.DrawContext[UIData]) {
		drawHeader(ctx)
		paints <- regionPaint{region: "header", data: ctx.Data, term: ctx.Term, bg: ctx.View.CellAt(0, 0).Style.Bg}
	}).Footer(footerRows, func(ctx frame.DrawContext[UIData]) {
		drawFooter(ctx)
		paints <- regionPaint{region: "footer", data: ctx.Data, term: ctx.Term}
	})
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		result, err := app.Run(ctx)
		if err == nil && (result.ProcessState == nil || result.ProcessState.ExitCode() != 0) {
			err = errors.New("child did not exit successfully")
		}
		done <- err
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(sessionTimeout):
			t.Error("demo session did not stop")
		}
	})
	ready := waitPaint(t, paints, func(p regionPaint) bool {
		return p.term.Terminal.Title == "demo-child" && p.term.Child.PID > 0
	})
	if ready.term.Viewport != (frame.Size{Cols: 98, Rows: 13}) || ready.term.Child.Executable == "" {
		t.Fatalf("metadata does not describe the real child viewport: %+v", ready.term)
	}
	negotiated := false
	for !negotiated {
		select {
		case s := <-states:
			negotiated = s.flags&ghostty.KittyKeyDisambiguate != 0
		case <-ctx.Done():
			t.Fatal("outer terminal was not asked to disambiguate Ctrl+number")
		}
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
			case state := <-states:
				if (state.corner == lipgloss.RoundedBorder().TopLeft) == enabled {
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
		{"\x1b[49;5u", 1, 0, true, frame.Size{Cols: 98, Rows: 13}, red, ""},
		{"\x1b[49;5u", 2, 0, true, frame.Size{Cols: 98, Rows: 13}, red, ""},
		{"\x1b[49;5u", 0, 0, true, frame.Size{Cols: 98, Rows: 13}, red, ""},
		{"\x1b[50;5u", 0, 1, true, frame.Size{Cols: 98, Rows: 13}, navy, ""},
		{"\x1b[50;5u", 0, 2, true, frame.Size{Cols: 98, Rows: 13}, teal, ""},
		{"\x1b[50;5u", 0, 0, true, frame.Size{Cols: 98, Rows: 13}, red, ""},
		{"\x1b[51;5u", 0, 0, false, frame.Size{Cols: 100, Rows: 15}, red, "13 98\n15 100\n"},
		{"\x1b[51;5u", 0, 0, true, frame.Size{Cols: 98, Rows: 13}, red, "13 98\n15 100\n13 98\n"},
	} {
		if change.sizeOutput != "" {
			// Discard old renders before checking the next actual border state.
			for len(states) > 0 {
				<-states
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
	// Selected release reports do not cycle/toggle a second time or reach the
	// child. Ordinary digits and the old function keys remain child input.
	if _, err := io.WriteString(master, "\x1b[49;5:3u\x1b[50;5:3u\x1b[51;5:3u"); err != nil {
		t.Fatal(err)
	}
	querySize("13 98\n15 100\n13 98\n13 98\n")
	released := waitPaint(t, paints, func(p regionPaint) bool { return p.term.Terminal.Title == "sized-4" })
	if released.data.Demo != 0 || released.data.Background != 0 || !released.data.Border {
		t.Fatalf("reported releases changed the demo: %+v", released.data)
	}
	if _, err := io.WriteString(master, "123\x1b[15~\x1b[17~ hello child\n"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
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
	master, slave := demoTTY(t)
	ctx, cancel := context.WithTimeout(context.Background(), sessionTimeout)
	defer cancel()
	ctx, quit := context.WithCancelCause(ctx)
	defer quit(nil)
	child := exec.Command("sh", "-c", "sleep 30")
	app := newDemoFrame(child, UIData{}, quit).Terminal(slave, slave)
	ready := make(chan struct{}, 1)
	app.Header(headerRows, func(ctx frame.DrawContext[UIData]) {
		drawHeader(ctx)
		if ctx.Term.Child.PID > 0 {
			select {
			case ready <- struct{}{}:
			default:
			}
		}
	})
	type outcome struct {
		result frame.Result
		err    error
	}
	done := make(chan outcome, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		result, err := app.Run(ctx)
		done <- outcome{result, err}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(sessionTimeout):
			t.Error("quit session did not stop")
		}
	})
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("child never painted its PID")
	}
	if _, err := master.Write([]byte{0x11}); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if !errors.Is(got.err, errDemoQuit) || got.result.ProcessState == nil || !got.result.ProcessState.Exited() && got.result.ProcessState.ExitCode() != -1 {
		t.Fatalf("quit lost cause or unreaped process: %+v, %v", got.result, got.err)
	}
	if got.result.DrainError != nil || got.result.CleanupError != nil {
		t.Fatalf("quit failed cleanup: %+v", got.result)
	}
}

func TestAgentDemoStartsPlainAndCanEnableChildBorder(t *testing.T) {
	master, slave := demoTTY(t)
	ctx, cancel := context.WithTimeout(context.Background(), sessionTimeout)
	defer cancel()
	ctx, quit := context.WithCancelCause(ctx)
	defer quit(nil)
	child := exec.Command("sh", "-c", "sleep 30")
	app := newDemoFrame(child, UIData{Agent: true}, quit).Terminal(slave, slave)
	paints := make(chan regionPaint, 32)
	app.Header(headerRows, func(ctx frame.DrawContext[UIData]) {
		drawHeader(ctx)
		paints <- regionPaint{data: ctx.Data, term: ctx.Term, bg: ctx.View.CellAt(0, 0).Style.Bg}
	})
	done := make(chan error, 1)
	go func() { _, err := app.Run(ctx); done <- err }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(sessionTimeout):
			t.Error("agent demo did not stop")
		}
	})
	initial := waitPaint(t, paints, func(p regionPaint) bool { return p.term.Child.PID > 0 })
	if initial.data.Border || initial.term.Viewport != (frame.Size{Cols: 100, Rows: 15}) || initial.bg != nil {
		t.Fatalf("agent defaults are not plain and borderless: %+v", initial)
	}
	if _, err := io.WriteString(master, "\x1b[51;5u"); err != nil {
		t.Fatal(err)
	}
	enabled := waitPaint(t, paints, func(p regionPaint) bool { return p.data.Border })
	if enabled.term.Viewport != (frame.Size{Cols: 98, Rows: 13}) || enabled.bg != nil {
		t.Fatalf("agent border toggle lost plain styling or geometry: %+v", enabled)
	}
}
