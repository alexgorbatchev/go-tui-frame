package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"

	helptree "github.com/alexgorbatchev/cobra-help-tree/v2"
	frame "github.com/alexgorbatchev/go-tui-frame"
)

var errDemoQuit = errors.New("quit requested by frame capture")

func runDemo(ctx context.Context, child *exec.Cmd, showcase, inheritTerminal bool) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	demo := newDemoSession(child, UIData{Agent: helptree.IsAgentMode()}, cancel)
	demo.frame.InheritTerminal(inheritTerminal)
	if showcase {
		stop := demo.startShowcase(ctx)
		defer stop()
	}
	result, err := demo.frame.Run(ctx)
	if errors.Is(err, errDemoQuit) {
		return errors.Join(result.DrainError, result.CleanupError)
	}
	if err != nil {
		return fmt.Errorf("run child frame: %w", err)
	}
	if result.ProcessState == nil {
		return errors.New("child frame returned without a process exit state")
	}
	code := result.ProcessState.ExitCode()
	if code < 0 {
		if status, ok := result.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			code = 128 + int(status.Signal())
		} else {
			code = 1
		}
	}
	if code != 0 {
		return &exitError{code: code, err: fmt.Errorf("child exited with status %d", code)}
	}
	return nil
}

type demoSession struct {
	mu     sync.Mutex
	frame  *frame.Frame[UIData]
	data   UIData
	cancel context.CancelCauseFunc
}

func newDemoSession(child *exec.Cmd, data UIData, cancel context.CancelCauseFunc) *demoSession {
	data.Border = !data.Agent
	app := frame.New(child, data).Header(headerRows, drawHeader).Footer(footerRows, drawFooter)
	app.Border(data.Border)
	demo := &demoSession{frame: app, data: data, cancel: cancel}
	app.Capture(demo.capture)
	return demo
}

func (d *demoSession) capture(input frame.Input) frame.Disposition {
	action := actionFor(input)
	switch action {
	case demoPass:
		return frame.Pass
	case demoRelease:
		return frame.Consume
	case demoQuit:
		d.cancel(errDemoQuit)
		return frame.Consume
	}
	if err := d.apply(action); err != nil {
		d.cancel(err)
	}
	return frame.Consume
}

func (d *demoSession) apply(action demoAction) error {
	// Keyboard capture and showcase events publish one ordered stream of values.
	d.mu.Lock()
	defer d.mu.Unlock()
	switch action {
	case demoNext:
		d.data.Demo = (d.data.Demo + 1) % demoCount
	case demoBackground:
		d.data.Background = (d.data.Background + 1) % backgroundCount
	case demoBorder:
		d.data.Border = !d.data.Border
		if err := d.frame.SetBorder(d.data.Border); err != nil {
			return fmt.Errorf("update child border: %w", err)
		}
	}
	if err := invalidateDemo(d.frame, d.data); err != nil {
		return fmt.Errorf("update demo regions: %w", err)
	}
	return nil
}

func invalidateDemo(app *frame.Frame[UIData], data UIData) error {
	return errors.Join(app.InvalidateHeader(data), app.InvalidateFooter(data))
}
