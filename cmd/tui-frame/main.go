package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() { os.Exit(execute()) }

func execute() int {
	// Losing the controlling terminal delivers SIGHUP. Go's default action would
	// exit before Frame.Run terminates the child's process groups and reaps it.
	ctx, stop := cancelOnSignal(context.Background(), syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cmd, err := newRootCommand()
	if err != nil {
		if _, writeErr := fmt.Fprintln(os.Stderr, err); writeErr != nil {
			return 1
		}
		return 1
	}
	if err := cmd.ExecuteContext(ctx); err != nil {
		var status *exitError
		if errors.As(err, &status) {
			return status.code
		}
		// The session's own error can precede the signal, as when a closed
		// terminal ends input before its SIGHUP arrives, so the status comes
		// from the context's cause rather than from err.
		var received *signalCause
		if errors.As(context.Cause(ctx), &received) {
			return signalExitCode(received.signal)
		}
		return 1
	}
	return 0
}

// signalCause is the cancellation cause for a received signal.
// signal.NotifyContext's cause records only the signal's name; the exit status
// needs its number.
type signalCause struct{ signal syscall.Signal }

func (c *signalCause) Error() string { return c.signal.String() + " signal received" }

// cancelOnSignal returns a context canceled with a *signalCause when one of
// sigs arrives. stop unregisters the signals and releases the context.
func cancelOnSignal(parent context.Context, sigs ...os.Signal) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)
	received := make(chan os.Signal, 1)
	signal.Notify(received, sigs...)
	go func() {
		select {
		case s := <-received:
			// os/signal delivers syscall.Signal values on Unix.
			cancel(&signalCause{signal: s.(syscall.Signal)})
		case <-ctx.Done():
		}
	}()
	return ctx, func() {
		signal.Stop(received)
		cancel(nil)
	}
}

// signalExitCode is the shell's status for a process ended by sig.
func signalExitCode(sig syscall.Signal) int { return 128 + int(sig) }
