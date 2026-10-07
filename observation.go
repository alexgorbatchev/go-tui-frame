package frame

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"sync"
	"time"
	"unsafe"

	"github.com/alexgorbatchev/go-tui-frame/v2/internal/emulator"
	"github.com/alexgorbatchev/go-tui-frame/v2/internal/process"
	uv "github.com/charmbracelet/ultraviolet"
)

const (
	observationQueueLimit = 64
	observationByteLimit  = 16 << 20
)

// eventDispatcher separates consumer work from terminal transport. The budget
// includes the current callback until it returns, as well as queued records;
// bytes is their combined weight. err holds the first callback failure, a
// panic or runtime.Goexit, after which no record reaches the callback.
type eventDispatcher struct {
	mu       sync.Mutex
	queue    chan eventRecord
	done     chan struct{}
	handler  func(Event)
	fail     context.CancelCauseFunc
	closed   bool
	err      error
	bytes    int
	sequence uint64
	offsets  map[EventKind]uint64
	kinds    eventMask
}

type eventMask uint16

const allEvents eventMask = 1<<10 - 1

func eventBit(kind EventKind) eventMask {
	switch kind {
	case OuterInput:
		return 1 << 0
	case ChildOutput:
		return 1 << 1
	case ChildInput:
		return 1 << 2
	case Started:
		return 1 << 3
	case Exited:
		return 1 << 4
	case Resized:
		return 1 << 5
	case Captured:
		return 1 << 6
	case StateChanged:
		return 1 << 7
	case Protocol:
		return 1 << 8
	case Routed:
		return 1 << 9
	}
	return 0
}

func selectEvents(kinds []EventKind) (eventMask, error) {
	var mask eventMask
	for _, kind := range kinds {
		bit := eventBit(kind)
		if bit == 0 {
			return 0, fmt.Errorf("unknown observation kind %q", kind)
		}
		mask |= bit
	}
	return mask, nil
}

// snapshotEvents selects the events that carry a Snapshot.
var snapshotEvents = eventBit(Started) | eventBit(Exited) | eventBit(Resized) | eventBit(StateChanged)

func (d *eventDispatcher) wants(kind EventKind) bool {
	return d.wantsAny(eventBit(kind))
}

// wantsAny reports whether any event in mask is selected.
func (d *eventDispatcher) wantsAny(mask eventMask) bool {
	return d != nil && d.kinds&mask != 0
}

type eventRecord struct {
	event  Event
	weight int
}

// newEventDispatcher delivers selected events to handler on a goroutine that
// no session defer covers. A handler panic or runtime.Goexit therefore becomes
// an error that cancels the session through fail, so shutdown still terminates
// the child and restores the terminal.
func newEventDispatcher(kinds eventMask, fail context.CancelCauseFunc, handler func(Event)) *eventDispatcher {
	if handler == nil || kinds == 0 {
		return nil
	}
	d := &eventDispatcher{queue: make(chan eventRecord, observationQueueLimit), done: make(chan struct{}),
		handler: handler, fail: fail, offsets: make(map[EventKind]uint64), kinds: kinds}
	go d.drain(true)
	return d
}

// drain receives admitted records until close, then closes done. It runs the
// callback on each record while delivering is true. Records admitted before a
// callback failure are still drained, releasing their weight, so close joins
// and the budget stays exact.
func (d *eventDispatcher) drain(delivering bool) {
	for record := range d.queue {
		if delivering {
			delivering = d.deliver(record)
		}
		d.release(record.weight)
	}
	close(d.done)
}

// deliver runs the callback for one record and reports whether delivery
// continues. runtime.Goexit cannot be recovered: after running the deferred
// calls, it ends this goroutine, which never returns to drain. The deferred
// call therefore stops delivery, releases the record, and starts another drain
// for the records still queued.
func (d *eventDispatcher) deliver(record eventRecord) bool {
	returned := false
	defer func() {
		// callObserver recovers panics, so only Goexit skips the assignment. A
		// nil recover value would not identify Goexit: with GODEBUG=panicnil=1,
		// recover returns nil for panic(nil) too, and this goroutine resumes.
		if returned {
			return
		}
		// Goexit runs this call on top of the callback's frames, so the stack
		// shows where the callback exited.
		d.stop(fmt.Errorf("%w\n\n%s", errObserverGoexit, debug.Stack()))
		d.release(record.weight)
		go d.drain(false)
	}()
	err := callObserver(d.handler, record.event)
	returned = true
	if err != nil {
		d.stop(err)
		return false
	}
	return true
}

// stop ends delivery with the callback failure err and cancels the session
// with it as the cause.
func (d *eventDispatcher) stop(err error) {
	d.mu.Lock()
	d.err = err
	d.mu.Unlock()
	d.fail(err)
}

// release returns a drained record's weight to the budget.
func (d *eventDispatcher) release(weight int) {
	d.mu.Lock()
	d.bytes -= weight
	d.mu.Unlock()
}

// callObserver runs one callback and converts its panic into an error carrying
// the panic value and the panicking goroutine's stack.
func callObserver(handler func(Event), ev Event) (err error) {
	returned := false
	defer func() {
		// The flag, not the recovered value, tells a panic from a return: with
		// GODEBUG=panicnil=1, recover stops panic(nil) and returns nil.
		if returned {
			return
		}
		// runtime.Goexit also skips the assignment, and recover returns nil for
		// it. Goexit then ends the goroutine without returning this error to
		// deliver, whose own flag reports the Goexit instead.
		err = observerPanicError(recover(), debug.Stack())
	}()
	handler(ev)
	returned = true
	return nil
}

// errObserverGoexit reports a callback that ended the dispatcher's goroutine
// with runtime.Goexit, as t.FailNow, t.Fatal, and t.SkipNow do.
var errObserverGoexit = errors.New("observer called runtime.Goexit")

func observerPanicError(value any, stack []byte) error {
	if err, ok := value.(error); ok {
		return fmt.Errorf("observer panicked: %w\n\n%s", err, stack)
	}
	return fmt.Errorf("observer panicked: %v\n\n%s", value, stack)
}

// failure returns the callback panic or runtime.Goexit that ended delivery, if
// any.
func (d *eventDispatcher) failure() error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}

func (d *eventDispatcher) emit(ev Event) error {
	if !d.wants(ev.Kind) {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return ErrSessionClosed
	}
	if d.err != nil {
		// No callback will run again. The session learns of the failure through
		// its canceled context; copying or queueing the record would only spend
		// the budget during shutdown.
		return nil
	}
	weight := eventWeight(ev)
	if !d.admits(weight) {
		return ErrObservationOverflow
	}
	d.sequence++
	ev.Sequence, ev.Offset = d.sequence, d.offsets[ev.Kind]
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	switch ev.Kind {
	case OuterInput, ChildOutput, ChildInput:
		d.offsets[ev.Kind] += uint64(len(ev.Bytes))
	}
	ev.Bytes = slices.Clone(ev.Bytes)
	if ev.Input != nil {
		in := *ev.Input
		in.Raw = slices.Clone(in.Raw)
		ev.Input = &in
	}
	if ev.Snapshot != nil {
		snap := cloneSnapshot(*ev.Snapshot)
		ev.Snapshot = &snap
	}
	if ev.Effect != nil {
		effect := emulator.CloneEffect(*ev.Effect)
		ev.Effect = &effect
	}
	d.bytes += weight
	d.queue <- eventRecord{event: ev, weight: weight} // Capacity was checked under the producer lock.
	return nil
}

// admits reports whether a record of weight fits beside the records already
// queued or executing. The byte budget bounds that backlog, not one record: a
// snapshot of a screen within maxScreenCells can outweigh the budget alone, so
// an idle dispatcher admits the next record whatever its weight. eventWeight
// counts each record's Event struct, so zero bytes means none is held.
func (d *eventDispatcher) admits(weight int) bool {
	if d.bytes == 0 {
		return true
	}
	return len(d.queue) < cap(d.queue) && weight <= observationByteLimit-d.bytes
}

func (d *eventDispatcher) close() {
	if d == nil {
		return
	}
	d.mu.Lock()
	if !d.closed {
		d.closed = true
		close(d.queue)
	}
	d.mu.Unlock()
	<-d.done
}

func eventWeight(ev Event) int {
	w := int(unsafe.Sizeof(ev)) + len(ev.Bytes) + len(ev.Origin)
	if ev.Error != nil {
		w += len(ev.Error.Error())
	}
	if ev.Input != nil {
		w += int(unsafe.Sizeof(*ev.Input)) + len(ev.Input.Raw)
		if ev.Input.Key != nil {
			key := ev.Input.Key.Key()
			w += int(unsafe.Sizeof(key)) + len(key.Text)
		}
	}
	if s := ev.Snapshot; s != nil {
		w += int(unsafe.Sizeof(*s)) + len(s.Child.Executable) + len(s.Child.Directory) +
			len(s.Terminal.Title) + len(s.Terminal.Directory)
		for _, values := range [][]string{s.Child.Args, s.Child.Environment} {
			w += len(values) * int(unsafe.Sizeof(""))
			for _, value := range values {
				w += len(value)
			}
		}
		if !sharesCellGrid(s.Terminal) {
			w += cellsWeight(s.Terminal.Cells)
		}
		w += nativeStateWeight(s.Terminal.Native)
		w += processWeight(s.Child.OperatingSystem)
		for _, p := range s.Child.SessionProcesses {
			w += processWeight(p)
		}
		if s.Child.SessionError != nil {
			w += len(s.Child.SessionError.Error())
		}
		if s.Child.PTY.Window != nil {
			w += int(unsafe.Sizeof(*s.Child.PTY.Window))
		}
		if s.Child.PTY.Settings != nil {
			w += int(unsafe.Sizeof(*s.Child.PTY.Settings))
		}
		w += fieldsWeight(s.Child.PTY.ForegroundGroup)
	}
	if ev.Effect != nil {
		w += effectWeight(*ev.Effect)
	}
	return w
}

func nativeStateWeight(s emulator.State) int {
	w := len(s.Title) + len(s.Directory) + len(s.Modes)*(int(unsafe.Sizeof(0))+int(unsafe.Sizeof(false))) + len(s.ModeErrors)*(int(unsafe.Sizeof(0))+int(unsafe.Sizeof(error(nil))))
	w += len(s.NativeCells) * int(unsafe.Sizeof(emulator.NativeCell{}))
	return w + cellsWeight(s.Cells)
}

func cellsWeight(cells []uv.Cell) int {
	w := 0
	for _, c := range cells {
		w += int(unsafe.Sizeof(c)) + len(c.Content) + len(c.Link.URL) + len(c.Link.Params)
	}
	return w
}

func effectWeight(e emulator.Effect) int {
	w := int(unsafe.Sizeof(e)) + len(e.Kind) + len(e.Text) + len(e.Bytes)
	if e.Unknown != nil {
		w += int(unsafe.Sizeof(*e.Unknown)) + len(e.Unknown.APC.Content) + len(e.Unknown.OSC.Content)
	}
	if e.ClipboardRead != nil {
		w += int(unsafe.Sizeof(*e.ClipboardRead)) + len(e.ClipboardRead.Name)
		for _, m := range e.ClipboardRead.MIMEs {
			w += int(unsafe.Sizeof(m)) + len(m)
		}
	}
	if e.ClipboardWrite != nil {
		w += int(unsafe.Sizeof(*e.ClipboardWrite)) + len(e.ClipboardWrite.Name)
		for _, c := range e.ClipboardWrite.Contents {
			w += int(unsafe.Sizeof(c)) + len(c.Data) + len(c.MIME)
		}
	}
	if e.Progress != nil {
		w += int(unsafe.Sizeof(*e.Progress))
	}
	if e.Notification != nil {
		w += int(unsafe.Sizeof(*e.Notification)) + len(e.Notification.Title) + len(e.Notification.Body)
	}
	if e.SemanticPrompt != nil {
		w += int(unsafe.Sizeof(*e.SemanticPrompt)) + len(e.SemanticPrompt.Command) + len(e.SemanticPrompt.Error)
	}
	return w
}

func processWeight(s process.Snapshot) int {
	w := int(unsafe.Sizeof(s))
	w += fieldsWeight(s.ParentPID, s.SessionID, s.ProcessGroup, s.ForegroundGroup, s.Resources.Threads)
	w += fieldsWeight(s.Name, s.State, s.Directory, s.Executable, s.StartIdentity, s.Status)
	w += fieldsWeight(s.Args, s.Environment)
	w += fieldsWeight(s.Resources.ResidentBytes, s.Resources.VirtualBytes)
	w += fieldsWeight(s.Resources.CPUUser, s.Resources.CPUSystem)
	w += fieldsWeight(s.Resources.Native)
	return w
}

func fieldsWeight[T any](fields ...Observation[T]) int {
	w := 0
	for _, f := range fields {
		w += len(f.Source)
		if f.Error != nil {
			w += len(f.Error.Error())
		}
		switch v := any(f.Value).(type) {
		case string:
			w += len(v)
		case []string:
			for _, s := range v {
				w += int(unsafe.Sizeof(s)) + len(s)
			}
		case map[string]uint64:
			for k := range v {
				w += int(unsafe.Sizeof(k)) + int(unsafe.Sizeof(uint64(0))) + len(k)
			}
		}
	}
	return w
}
