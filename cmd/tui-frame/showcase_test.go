package main

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	frame "github.com/alexgorbatchev/go-tui-frame"
	"github.com/alexgorbatchev/go-tui-frame/internal/emulator"
)

func TestShowcaseUpdatesIdleChildFrame(t *testing.T) {
	_, slave := demoTTY(t)
	ctx, cancel := context.WithTimeout(context.Background(), sessionTimeout)
	defer cancel()
	ctx, quit := context.WithCancelCause(ctx)
	defer quit(nil)
	child := exec.Command("sh", "-c", "printf '\033]2;idle-child\007'; sleep 30")
	demo := newDemoSession(child, UIData{}, quit)
	app := demo.frame.Terminal(slave, slave)
	paints := make(chan regionPaint, 64)
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
			t.Error("showcase session did not stop")
		}
	})
	waitPaint(t, paints, func(p regionPaint) bool { return p.term.Terminal.Title == "idle-child" })
	played := make(chan error, 1)
	go func() { played <- demo.playShowcase(ctx, 100*time.Millisecond) }()
	for _, want := range []struct {
		layout, background int
		border             bool
		colour             string
	}{
		{1, 0, true, red},
		{1, 1, true, navy},
		{1, 2, true, teal},
		{1, 2, false, teal},
		{2, 2, false, teal},
		{2, 2, true, teal},
		{2, 0, true, red},
		{0, 0, true, red},
	} {
		p := waitPaint(t, paints, func(p regionPaint) bool {
			return p.data.Demo == want.layout && p.data.Background == want.background && p.data.Border == want.border
		})
		cols, rows := 100, 15
		if want.border {
			cols, rows = 98, 13
		}
		window := p.term.Child.PTY.Window
		if p.term.Terminal.Title != "idle-child" || !sameColor(p.bg, lipgloss.Color(want.colour)) ||
			p.term.Viewport != (frame.Size{Cols: cols, Rows: rows}) || window == nil ||
			int(window.Col) != cols || int(window.Row) != rows {
			t.Fatalf("showcase did not paint/resize idle child: data=%+v background=%v snapshot=%+v", p.data, p.bg, p.term)
		}
	}
	if err := <-played; err != nil {
		t.Fatal(err)
	}
}

func TestLayoutUpdatesPreserveChildOnOuterTerminal(t *testing.T) {
	states := make(chan string, 128)
	_, slave := demoTTYObserved(t, func(state emulator.State) {
		var text strings.Builder
		for _, cell := range state.Cells {
			text.WriteString(cell.Content)
		}
		select {
		case states <- text.String():
		default:
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), sessionTimeout)
	defer cancel()
	ctx, quit := context.WithCancelCause(ctx)
	defer quit(nil)
	child := exec.Command("sh", "-c", "printf 'CHILD-MARKER'; sleep 30")
	demo := newDemoSession(child, UIData{}, quit)
	demo.frame.Terminal(slave, slave)
	done := make(chan error, 1)
	go func() { _, err := demo.frame.Run(ctx); done <- err }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(sessionTimeout):
			t.Error("child display check did not stop")
		}
	})
	waitLayout := func(layout int) {
		t.Helper()
		for {
			select {
			case text := <-states:
				if strings.Contains(text, demoName(layout)) && strings.Contains(text, "CHILD-MARKER") {
					return
				}
			case <-ctx.Done():
				t.Fatalf("child disappeared on the physical terminal for layout %s", demoName(layout))
			}
		}
	}
	for _, layout := range []int{0, 1, 2, 0} {
		waitLayout(layout)
		if err := demo.apply(demoNext); err != nil {
			t.Fatal(err)
		}
	}
}

func TestShowcaseStopsOnCancellationAndClosedFrame(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(map[bool]string{false: "canceled", true: "closed"}[closed], func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			demo := newDemoSession(exec.Command("sh"), UIData{}, cancel)
			if closed {
				// A real configuration error permanently closes this controller.
				demo.frame.Header(0, drawHeader)
				if _, err := demo.frame.Run(ctx); err == nil {
					t.Fatal("invalid configuration unexpectedly succeeded")
				}
			} else {
				cancel(context.Canceled)
			}
			err := demo.playShowcase(ctx, time.Millisecond)
			if closed && !errors.Is(err, frame.ErrSessionClosed) || !closed && err != nil {
				t.Fatalf("playback shutdown: %v", err)
			}
		})
	}
}
