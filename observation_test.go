package frame

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
)

func TestObservationsOwnBytesAndSnapshotsAndStayOrdered(t *testing.T) {
	var got []Event
	d, entered, release := pausedDispatcher(t, func(ev Event) { got = append(got, ev) })
	first := Event{Kind: ChildOutput, Bytes: []byte("one"), Snapshot: &Snapshot{
		Child:    ChildSnapshot{Args: []string{"original"}},
		Terminal: TerminalSnapshot{Cells: []uv.Cell{{Content: "a", Width: 1}}},
	}}
	if err := d.emit(first); err != nil {
		t.Fatal(err)
	}
	<-entered
	first.Bytes[0], first.Snapshot.Child.Args[0], first.Snapshot.Terminal.Cells[0].Content = 'x', "changed", "x"
	if err := d.emit(Event{Kind: OuterInput, Bytes: []byte("key")}); err != nil {
		t.Fatal(err)
	}
	if err := d.emit(Event{Kind: ChildOutput, Bytes: []byte("two")}); err != nil {
		t.Fatal(err)
	}
	release()
	d.close()
	if len(got) != 3 || string(got[0].Bytes) != "one" || got[0].Snapshot.Child.Args[0] != "original" ||
		got[0].Snapshot.Terminal.Cells[0].Content != "a" {
		t.Fatalf("observation borrowed live storage: %#v", got)
	}
	for i, ev := range got {
		if ev.Sequence != uint64(i+1) || ev.Time.IsZero() {
			t.Fatalf("bad event order/timestamp: %#v", ev)
		}
	}
	if got[0].Offset != 0 || got[1].Offset != 0 || got[2].Offset != 3 {
		t.Fatalf("stream offsets crossed streams: %#v", got)
	}
}

func TestSlowObserverReportsOverflowWithoutBlockingProducer(t *testing.T) {
	d, entered, release := pausedDispatcher(t, func(Event) {})
	if err := d.emit(Event{Kind: Started}); err != nil {
		t.Fatal(err)
	}
	<-entered
	// A single excessive record must fail before allocating its durable copy.
	done := make(chan error, 1)
	go func() { done <- d.emit(Event{Kind: ChildOutput, Bytes: make([]byte, observationByteLimit+1)}) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrObservationOverflow) {
			t.Fatalf("overflow error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow observer blocked producer")
	}
	release()
	d.close()
}

func TestObserverQueueCountIsBoundedAndShutdownRejects(t *testing.T) {
	d, entered, release := pausedDispatcher(t, func(Event) {})
	if err := d.emit(Event{Kind: Started}); err != nil {
		t.Fatal(err)
	}
	<-entered
	for range observationQueueLimit {
		if err := d.emit(Event{Kind: ChildOutput, Bytes: []byte("data")}); err != nil {
			t.Fatalf("queue rejected within capacity: %v", err)
		}
	}
	if err := d.emit(Event{Kind: ChildOutput, Bytes: []byte("overflow")}); !errors.Is(err, ErrObservationOverflow) {
		t.Fatalf("queue count overflow = %v", err)
	}
	release()
	d.close()
	if err := d.emit(Event{Kind: Exited}); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("closed observer accepted record: %v", err)
	}
}

func pausedDispatcher(t *testing.T, handler func(Event)) (*eventDispatcher, <-chan struct{}, func()) {
	t.Helper()
	entered, resume := make(chan struct{}), make(chan struct{})
	var started, released sync.Once
	d := newEventDispatcher(allEvents, func(ev Event) {
		started.Do(func() { close(entered); <-resume })
		handler(ev)
	})
	release := func() { released.Do(func() { close(resume) }) }
	t.Cleanup(func() { release(); d.close() })
	return d, entered, release
}

func TestDisabledObservationDoesNotRetainOrDeliver(t *testing.T) {
	d := newEventDispatcher(allEvents, nil)
	if d != nil {
		t.Fatal("disabled observer allocated dispatcher")
	}
	if err := d.emit(Event{Kind: Started}); err != nil {
		t.Fatal(err)
	}
	d.close()
}

func TestObserverMutationDoesNotChangeQueueAccounting(t *testing.T) {
	d := newEventDispatcher(allEvents, func(ev Event) {
		ev.Snapshot.Terminal.Title = "a longer observer-owned title"
		ev.Input.Raw = nil
	})
	if err := d.emit(Event{Kind: Started, Snapshot: &Snapshot{}, Input: &Input{Raw: []byte("key")}}); err != nil {
		t.Fatal(err)
	}
	d.close()
	if d.bytes != 0 {
		t.Fatalf("consumer mutation changed accounting: %d outstanding bytes", d.bytes)
	}
}

func TestObservationSubscriptionsValidateBeforeStarting(t *testing.T) {
	for _, tt := range []struct {
		name    string
		kinds   []EventKind
		wantErr bool
	}{{"empty", nil, false}, {"duplicates", []EventKind{ChildOutput, ChildOutput}, false}, {"unknown", []EventKind{"unknown"}, true}} {
		t.Run(tt.name, func(t *testing.T) {
			f := New(exec.Command("sh"), struct{}{}).ObserveEvents(tt.kinds, func(Event) {})
			if err := f.begin(context.Background()); (err != nil) != tt.wantErr {
				t.Fatalf("subscription configuration error = %v", err)
			}
			d := newEventDispatcher(f.observedKinds, f.observe)
			if len(tt.kinds) == 0 && d != nil {
				t.Fatal("empty subscription allocated a dispatcher")
			}
			d.close()
			if f.cmd.Process != nil {
				t.Fatal("subscription validation started the child")
			}
		})
	}
}

func TestFilteredEventsDoNotConsumeQueueBudget(t *testing.T) {
	d := newEventDispatcher(eventBit(ChildOutput), func(Event) {})
	defer d.close()
	if err := d.emit(Event{Kind: OuterInput, Bytes: make([]byte, observationByteLimit+1)}); err != nil {
		t.Fatalf("excluded event consumed queue budget: %v", err)
	}
	snap := Snapshot{Terminal: TerminalSnapshot{Cells: []uv.Cell{{Content: "state", Width: 1}}}}
	if allocs := testing.AllocsPerRun(100, func() {
		if err := d.emit(Event{Kind: StateChanged, Snapshot: &snap}); err != nil {
			t.Fatal(err)
		}
	}); allocs != 0 {
		t.Fatalf("excluded event allocated %.0f times", allocs)
	}
}
