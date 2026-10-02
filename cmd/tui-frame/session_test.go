package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"

	frame "github.com/alexgorbatchev/go-tui-frame"
	"github.com/alexgorbatchev/go-tui-frame/internal/emulator"
	"github.com/creack/pty"
	"golang.org/x/term"
)

const sessionTimeout = 10 * time.Second

func demoTTY(t *testing.T) (*os.File, *os.File) {
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
	demo   int
	term   frame.Snapshot
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

func TestDemoCaptureRepaintsBothRegionsAndPassesChildInput(t *testing.T) {
	master, slave := demoTTY(t)
	before, err := term.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	inputFile := filepath.Join(projectTempDir(t), "child-input")
	child := exec.Command("sh", "-c", `stty -echo; printf '\033]2;demo-child\007'; IFS= read -r line; printf '%s' "$line" > "$1"`, "sh", inputFile)
	ctx, cancel := context.WithTimeout(context.Background(), sessionTimeout)
	defer cancel()
	ctx, quit := context.WithCancelCause(ctx)
	defer quit(nil)
	app := newDemoFrame(child, UIData{}, quit).Terminal(slave, slave)
	paints := make(chan regionPaint, 64)
	app.Header(headerRows, func(ctx frame.DrawContext[UIData]) {
		drawHeader(ctx)
		paints <- regionPaint{"header", ctx.Data.Demo, ctx.Term}
	}).Footer(footerRows, func(ctx frame.DrawContext[UIData]) {
		drawFooter(ctx)
		paints <- regionPaint{"footer", ctx.Data.Demo, ctx.Term}
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
	for _, change := range []struct {
		key  string
		demo int
	}{{"\x1b[17~", 1}, {"\x1b[17~", 2}, {"\x1b[15~", 1}} {
		if _, err := io.WriteString(master, change.key); err != nil {
			t.Fatal(err)
		}
		seen := make(map[string]bool)
		for len(seen) < 2 {
			p := waitPaint(t, paints, func(p regionPaint) bool { return p.demo == change.demo })
			seen[p.region] = true
		}
	}
	if _, err := io.WriteString(master, "hello child\n"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(inputFile)
	if err != nil || string(got) != "hello child" {
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
