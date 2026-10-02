package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"

	helptree "github.com/alexgorbatchev/cobra-help-tree/v2"
	frame "github.com/alexgorbatchev/go-tui-frame"
)

var errDemoQuit = errors.New("quit requested by frame capture")

func runDemo(ctx context.Context, child *exec.Cmd) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	app := newDemoFrame(child, UIData{Agent: helptree.IsAgentMode()}, cancel)
	result, err := app.Run(ctx)
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

func newDemoFrame(child *exec.Cmd, data UIData, cancel context.CancelCauseFunc) *frame.Frame[UIData] {
	data.Border = !data.Agent
	app := frame.New(child, data).Header(headerRows, drawHeader).Footer(footerRows, drawFooter)
	app.Border(data.Border)
	app.Capture(func(input frame.Input) frame.Disposition {
		switch actionFor(input) {
		case demoPass:
			return frame.Pass
		case demoRelease:
			return frame.Consume
		case demoQuit:
			cancel(errDemoQuit)
			return frame.Consume
		case demoNext:
			data.Demo = (data.Demo + 1) % demoCount
		case demoBackground:
			data.Background = (data.Background + 1) % backgroundCount
		case demoBorder:
			data.Border = !data.Border
			if err := app.SetBorder(data.Border); err != nil {
				cancel(fmt.Errorf("update child border: %w", err))
				return frame.Consume
			}
		}
		if err := invalidateDemo(app, data); err != nil {
			cancel(fmt.Errorf("update demo regions: %w", err))
		}
		return frame.Consume
	})
	return app
}

func invalidateDemo(app *frame.Frame[UIData], data UIData) error {
	return errors.Join(app.InvalidateHeader(data), app.InvalidateFooter(data))
}
