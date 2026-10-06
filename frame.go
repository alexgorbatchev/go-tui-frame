package frame

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
)

type edge uint8

const (
	header edge = iota
	footer
	left
	right
	edgeCount
)

type phase uint8

const (
	configuring phase = iota
	running
	closed
)

type region[T any] struct {
	size   int
	draw   func(DrawContext[T])
	data   T
	dirty  bool
	canvas *uv.ScreenBuffer
}

// Frame configures one child session and accepts concurrent region updates.
// Configure before Run; Invalidate methods and SetBorder accept concurrent updates.
type Frame[T any] struct {
	mu              sync.Mutex
	cmd             *exec.Cmd
	initial         T
	regions         [edgeCount]region[T]
	border          bool
	inheritTerminal bool
	layoutDirty     bool
	input           *os.File
	output          *os.File
	capture         func(Input) Disposition
	observe         func(Event)
	observedKinds   eventMask
	state           phase
	configErr       error
	wake            chan struct{}
	// probeTimeout bounds the startup capability queries. Tests raise it to
	// keep a terminal that answers late within the deadline.
	probeTimeout time.Duration
}

// New creates a controller without starting the command or touching a terminal.
func New[T any](cmd *exec.Cmd, initial T) *Frame[T] {
	return &Frame[T]{cmd: cmd, initial: initial, input: os.Stdin, output: os.Stdout, inheritTerminal: true,
		wake: make(chan struct{}, 1), probeTimeout: capabilityTimeout}
}

// Header reserves rows across the top of the outer terminal.
func (f *Frame[T]) Header(rows int, draw func(DrawContext[T])) *Frame[T] {
	return f.configureRegion(header, rows, draw)
}

// Footer reserves rows across the bottom of the outer terminal.
func (f *Frame[T]) Footer(rows int, draw func(DrawContext[T])) *Frame[T] {
	return f.configureRegion(footer, rows, draw)
}

// Left reserves columns between the header and footer.
func (f *Frame[T]) Left(cols int, draw func(DrawContext[T])) *Frame[T] {
	return f.configureRegion(left, cols, draw)
}

// Right reserves columns between the header and footer.
func (f *Frame[T]) Right(cols int, draw func(DrawContext[T])) *Frame[T] {
	return f.configureRegion(right, cols, draw)
}

func (f *Frame[T]) configureRegion(e edge, size int, draw func(DrawContext[T])) *Frame[T] {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state != configuring {
		f.configErr = ErrConfigurationFrozen
		f.notify()
		return f
	}
	if size <= 0 || draw == nil {
		f.configErr = fmt.Errorf("region requires positive dimensions and a drawing callback")
		return f
	}
	f.regions[e] = region[T]{size: size, draw: draw, data: f.initial, dirty: true}
	return f
}

// Border declares a one-cell border around the child viewport.
func (f *Frame[T]) Border(enabled bool) *Frame[T] {
	f.configure(func() { f.border = enabled })
	return f
}

// SetBorder schedules a change to the child viewport's one-cell border.
// It is safe before and during Run. Pending changes coalesce; acceptance does
// not acknowledge painting. An inset that cannot fit fails the running session
// with ErrViewportTooSmall, following the outer-resize contract.
func (f *Frame[T]) SetBorder(enabled bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == closed {
		return ErrSessionClosed
	}
	if f.border != enabled {
		f.border, f.layoutDirty = enabled, true
		f.notify()
	}
	return nil
}

func (f *Frame[T]) takeLayoutChange() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	changed := f.layoutDirty
	f.layoutDirty = false
	return changed
}

// Terminal selects the input and output terminals. The session borrows them;
// it restores terminal state and never closes the caller's files.
func (f *Frame[T]) Terminal(input, output *os.File) *Frame[T] {
	f.configure(func() { f.input, f.output = input, output })
	return f
}

// InheritTerminal selects whether the child starts with reported outer-terminal
// preferences and the original PTY line discipline. The default is true.
// Capability negotiation and terminal restoration remain required in either mode.
func (f *Frame[T]) InheritTerminal(enabled bool) *Frame[T] {
	f.configure(func() { f.inheritTerminal = enabled })
	return f
}

// Capture explicitly filters recognized keyboard events before child routing.
func (f *Frame[T]) Capture(handler func(Input) Disposition) *Frame[T] {
	f.configure(func() { f.capture = handler })
	return f
}

// Observe receives durable observations without consuming input.
// A handler panic ends the session as cancellation does, and Run returns an
// error with the panic value and stack. A handler that calls runtime.Goexit,
// as t.FailNow and t.Fatal do, ends the session the same way, and Run returns
// an error that names runtime.Goexit, with the stack.
func (f *Frame[T]) Observe(handler func(Event)) *Frame[T] {
	f.configure(func() { f.observe, f.observedKinds = handler, allEvents })
	return f
}

// ObserveEvents receives only the selected event kinds. An empty selection
// disables observations; unknown kinds fail configuration when Run begins.
// StateChanged retains one owned snapshot per child-output read when selected.
// A handler panic ends the session as cancellation does, and Run returns an
// error with the panic value and stack. A handler that calls runtime.Goexit,
// as t.FailNow and t.Fatal do, ends the session the same way, and Run returns
// an error that names runtime.Goexit, with the stack.
func (f *Frame[T]) ObserveEvents(kinds []EventKind, handler func(Event)) *Frame[T] {
	f.configure(func() {
		mask, err := selectEvents(kinds)
		if err != nil {
			f.configErr = err
			return
		}
		f.observe, f.observedKinds = handler, mask
	})
	return f
}

func (f *Frame[T]) configure(apply func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state != configuring {
		f.configErr = ErrConfigurationFrozen
		f.notify()
		return
	}
	apply()
}

// InvalidateHeader replaces the header payload and schedules drawing.
func (f *Frame[T]) InvalidateHeader(data T) error { return f.invalidate(header, data) }

// InvalidateFooter replaces the footer payload and schedules drawing.
func (f *Frame[T]) InvalidateFooter(data T) error { return f.invalidate(footer, data) }

// InvalidateLeft replaces the left-region payload and schedules drawing.
func (f *Frame[T]) InvalidateLeft(data T) error { return f.invalidate(left, data) }

// InvalidateRight replaces the right-region payload and schedules drawing.
func (f *Frame[T]) InvalidateRight(data T) error { return f.invalidate(right, data) }

func (f *Frame[T]) invalidate(e edge, data T) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == closed {
		return ErrSessionClosed
	}
	r := &f.regions[e]
	if r.draw == nil {
		return ErrRegionNotConfigured
	}
	r.data, r.dirty = data, true
	f.notify()
	return nil
}

func (f *Frame[T]) notify() {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *Frame[T]) close() {
	f.mu.Lock()
	f.state = closed
	f.mu.Unlock()
}
