package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	frame "github.com/alexgorbatchev/go-tui-frame"
)

const showcaseInterval = 2 * time.Second

func (d *demoSession) startShowcase(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	ready := make(chan struct{})
	// Started arrives once per Run, and a Frame runs one session. Selecting
	// only Started keeps child output, protocol effects and state snapshots out
	// of the bounded observation queue, which they would overflow whenever the
	// delivery goroutine falls behind, ending the session.
	d.frame.ObserveEvents([]frame.EventKind{frame.Started}, func(frame.Event) { close(ready) })
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			return
		case <-ready:
		}
		if err := d.playShowcase(ctx, showcaseInterval); err != nil && !errors.Is(err, frame.ErrSessionClosed) {
			d.cancel(fmt.Errorf("play frame showcase: %w", err))
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func (d *demoSession) playShowcase(ctx context.Context, interval time.Duration) error {
	// These are application events, using the same actions as keyboard capture.
	// A finite sequence demonstrates invalidation without a redraw/polling loop.
	actions := []demoAction{
		demoNext, demoBackground, demoBackground, demoBorder,
		demoNext, demoBorder, demoBackground, demoNext,
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for _, action := range actions {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		if err := d.apply(action); err != nil {
			return err
		}
		timer.Reset(interval)
	}
	return nil
}
