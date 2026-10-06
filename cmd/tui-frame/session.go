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

// childExit carries a child's nonzero exit status out of the command. The
// status is the run's result rather than a wrapper failure, so execute returns
// it as the process status without reporting a diagnostic.
type childExit struct{ code int }

func (e *childExit) Error() string { return fmt.Sprintf("child exited with status %d", e.code) }

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
	if failure := sessionFailure(err); failure != nil {
		return failure
	}
	if errors.Is(err, errDemoQuit) {
		// The frame terminated the child on request, so its status records that
		// termination rather than a result of its own.
		return nil
	}
	if result.ProcessState == nil {
		return errors.New("child frame returned without a process exit state")
	}
	code := result.ProcessState.ExitCode()
	if code < 0 {
		if status, ok := result.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			code = signalExitCode(status.Signal())
		} else {
			code = 1
		}
	}
	if code != 0 {
		return &childExit{code: code}
	}
	return nil
}

// sessionFailure reports err, the error Frame.Run returned, without the quit
// request, or nil when the quit was its only error. Frame.Run joins the
// context's cause with the later signal, render, deadline, read, drain, and
// cleanup errors, so a quit fails when any of those fails.
func sessionFailure(err error) error {
	if rest := withoutQuit(err); rest != nil {
		return fmt.Errorf("run child frame: %w", rest)
	}
	return nil
}

// withoutQuit removes the errDemoQuit leaves from the joined errors in err.
// The quit enters that tree only as the context's cause, a direct member of a
// join, so wrapping nodes are not searched. Branches without the quit stay
// intact, keeping their messages and wrapped errors.
func withoutQuit(err error) error {
	if err == errDemoQuit {
		return nil
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok || !errors.Is(err, errDemoQuit) {
		return err
	}
	var rest []error
	for _, member := range joined.Unwrap() {
		rest = append(rest, withoutQuit(member))
	}
	return errors.Join(rest...)
}

type demoSession struct {
	mu     sync.Mutex
	frame  *frame.Frame[UIData]
	data   UIData
	cancel context.CancelCauseFunc
	// keys belongs to capture, which the session calls inline.
	keys keyPrefix
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
	action, disposition := d.keys.route(input)
	switch action {
	case demoNone:
	case demoQuit:
		d.cancel(errDemoQuit)
	default:
		if err := d.apply(action); err != nil {
			d.cancel(err)
		}
	}
	return disposition
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
