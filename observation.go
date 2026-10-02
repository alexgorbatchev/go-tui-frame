package frame

import (
	"slices"
	"sync"
	"time"
	"unsafe"

	"github.com/alexgorbatchev/go-tui-frame/internal/emulator"
	"github.com/alexgorbatchev/go-tui-frame/internal/process"
)

const (
	observationQueueLimit = 64
	observationByteLimit  = 16 << 20
)

// eventDispatcher separates consumer work from terminal transport. The budget
// includes the current callback until it returns, as well as queued records.
type eventDispatcher struct {
	mu       sync.Mutex
	queue    chan eventRecord
	done     chan struct{}
	closed   bool
	bytes    int
	sequence uint64
	offsets  map[EventKind]uint64
}

type eventRecord struct {
	event  Event
	weight int
}

func newEventDispatcher(handler func(Event)) *eventDispatcher {
	if handler == nil {
		return nil
	}
	d := &eventDispatcher{queue: make(chan eventRecord, observationQueueLimit), done: make(chan struct{}),
		offsets: make(map[EventKind]uint64)}
	go func() {
		defer close(d.done)
		for record := range d.queue {
			handler(record.event)
			d.mu.Lock()
			d.bytes -= record.weight
			d.mu.Unlock()
		}
	}()
	return d
}

func (d *eventDispatcher) emit(ev Event) error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return ErrSessionClosed
	}
	weight := eventWeight(ev)
	if len(d.queue) == cap(d.queue) || weight > observationByteLimit-d.bytes {
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
		for _, cell := range s.Terminal.Cells {
			w += int(unsafe.Sizeof(cell)) + len(cell.Content) + len(cell.Link.URL) + len(cell.Link.Params)
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
	for _, c := range s.Cells {
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
