package frame

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

func TestBeginValidatesBeforeTerminalMutation(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Frame[string])
	}{
		{"nil command", func(f *Frame[string]) { f.cmd = nil }},
		{"started command", func(f *Frame[string]) { f.cmd.Process = &os.Process{Pid: 123} }},
		{"preassigned stdin", func(f *Frame[string]) { f.cmd.Stdin = os.Stdin }},
		{"preassigned stdout", func(f *Frame[string]) { f.cmd.Stdout = os.Stdout }},
		{"preassigned stderr", func(f *Frame[string]) { f.cmd.Stderr = os.Stderr }},
		{"custom cancel", func(f *Frame[string]) { f.cmd.Cancel = func() error { return nil } }},
		{"foreground group", func(f *Frame[string]) { f.cmd.SysProcAttr = &syscall.SysProcAttr{Foreground: true} }},
		{"detached terminal", func(f *Frame[string]) { f.cmd.SysProcAttr = &syscall.SysProcAttr{Noctty: true} }},
		{"no program", func(f *Frame[string]) { f.cmd.Path = "" }},
		{"nil terminal", func(f *Frame[string]) { f.Terminal(nil, nil) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := New(exec.Command("sh"), "ready")
			tt.edit(f)
			if err := f.begin(context.Background()); err == nil {
				t.Fatal("invalid configuration accepted")
			}
			if err := f.begin(context.Background()); !errors.Is(err, ErrAlreadyRun) {
				t.Fatalf("failed session reusable: %v", err)
			}
		})
	}
}

func TestBeginFreezesConfigurationAndHonorsCancellation(t *testing.T) {
	f := New(exec.Command("sh"), "ready").Header(1, func(DrawContext[string]) {})
	if err := f.begin(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.Border(true)
	if f.border || !errors.Is(f.configurationError(), ErrConfigurationFrozen) {
		t.Fatal("configuration mutated after session began")
	}
	if err := f.InvalidateHeader("live"); err != nil {
		t.Fatalf("live update rejected after configuration freeze: %v", err)
	}
	errCallerQuit := errors.New("caller quit")
	tests := []struct {
		name     string
		canceled func() context.Context
		want     error
	}{
		{"plain cancel", func() context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}, context.Canceled},
		// A caller tells its own cancellation apart by the cause, so a context
		// canceled before Run begins must report it as later cancellations do.
		{"cancel with cause", func() context.Context {
			ctx, cancel := context.WithCancelCause(context.Background())
			cancel(errCallerQuit)
			return ctx
		}, errCallerQuit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := New(exec.Command("sh"), "ready")
			if _, err := f.Run(tt.canceled()); !errors.Is(err, tt.want) || f.cmd.Process != nil {
				t.Fatalf("canceled startup = %v, want %v; process = %v", err, tt.want, f.cmd.Process)
			}
		})
	}
}
