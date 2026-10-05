package frame

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"testing"
	"time"
	"unsafe"

	uv "github.com/charmbracelet/ultraviolet"
	"golang.org/x/sys/unix"
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

func TestObservationBudgetBoundsBacklogRatherThanLoneRecords(t *testing.T) {
	oversized := oversizedStarted(t)
	for _, tt := range []struct {
		name    string
		held    *Event // executing when next arrives; nil leaves the dispatcher idle
		next    Event
		wantErr error
	}{
		{"idle dispatcher admits oversized snapshot", nil, oversized, nil},
		{"executing callback rejects oversized snapshot", &Event{Kind: Started}, oversized, ErrObservationOverflow},
		{"executing oversized snapshot rejects small record", &oversized, Event{Kind: ChildOutput, Bytes: []byte("data")}, ErrObservationOverflow},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got []Event
			d, entered, release := pausedDispatcher(t, func(ev Event) { got = append(got, ev) })
			var want []Event
			if tt.held != nil {
				if err := d.emit(*tt.held); err != nil {
					t.Fatalf("idle dispatcher rejected held %s record: %v", tt.held.Kind, err)
				}
				<-entered
				want = append(want, *tt.held)
			}
			if err := d.emit(tt.next); !errors.Is(err, tt.wantErr) {
				t.Fatalf("emit %s weighing %d bytes = %v, want %v", tt.next.Kind, eventWeight(tt.next), err, tt.wantErr)
			}
			if tt.wantErr == nil {
				want = append(want, tt.next)
			}
			release()
			d.close()
			if len(got) != len(want) {
				t.Fatalf("delivered %d records, want %d", len(got), len(want))
			}
			for i := range want {
				if g, w := recordSummary(got[i]), recordSummary(want[i]); g != w {
					t.Fatalf("delivered record %d = %s, want %s", i, g, w)
				}
			}
		})
	}
}

// oversizedStarted returns a Started record whose snapshot alone outweighs the
// observation budget while its viewport stays within maxScreenCells.
func oversizedStarted(t *testing.T) Event {
	t.Helper()
	cells := make([]uv.Cell, observationByteLimit/int(unsafe.Sizeof(uv.Cell{}))+1)
	if len(cells) > maxScreenCells {
		t.Fatalf("oversized snapshot needs %d cells, beyond the %d-cell screen budget", len(cells), maxScreenCells)
	}
	cells[len(cells)-1] = uv.Cell{Content: "z", Width: 1}
	ev := Event{Kind: Started, Snapshot: &Snapshot{Terminal: TerminalSnapshot{Cells: cells}}}
	if w := eventWeight(ev); w <= observationByteLimit {
		t.Fatalf("snapshot weighs %d bytes, want more than %d", w, observationByteLimit)
	}
	return ev
}

func recordSummary(ev Event) string {
	cells, last := 0, ""
	if ev.Snapshot != nil {
		cells = len(ev.Snapshot.Terminal.Cells)
		if cells > 0 {
			last = ev.Snapshot.Terminal.Cells[cells-1].Content
		}
	}
	return fmt.Sprintf("%s bytes=%q cells=%d last=%q", ev.Kind, ev.Bytes, cells, last)
}

func TestRunDeliversStartedSnapshotLargerThanObservationBudget(t *testing.T) {
	// 131,072 child cells stay within maxScreenCells, yet one Started snapshot
	// of them outweighs the whole observation budget.
	h := newSizedHarness(t, 512, 256)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	weights := make(chan int, 1)
	app := New(childCommand(t), struct{}{}).Terminal(h.slave, h.slave).ObserveEvents([]EventKind{Started}, func(e Event) {
		weights <- eventWeight(e)
	})
	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() { r, err := app.Run(ctx); done <- outcome{r, err} }()
	select {
	case w := <-weights:
		if w <= observationByteLimit {
			t.Fatalf("Started weighed %d bytes, want more than the %d-byte budget", w, observationByteLimit)
		}
	case got := <-done:
		t.Fatalf("Run ended before Started was delivered: %v", got.err)
	case <-ctx.Done():
		t.Fatal("Started was not delivered", ctx.Err())
	}
	awaitText(t, h, "child")
	if _, err := unix.Write(h.fd, []byte("q")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil || got.result.ProcessState == nil || !got.result.ProcessState.Success() {
			t.Fatalf("Run = %#v, %v", got.result, got.err)
		}
	case <-ctx.Done():
		t.Fatal("Run did not finish", ctx.Err())
	}
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
	return pausedSelectedDispatcher(t, allEvents, handler)
}

// pausedSelectedDispatcher observes kinds and holds the first delivered
// record's callback until release, so later records meet a non-idle budget.
func pausedSelectedDispatcher(t *testing.T, kinds eventMask, handler func(Event)) (*eventDispatcher, <-chan struct{}, func()) {
	t.Helper()
	entered, resume := make(chan struct{}), make(chan struct{})
	var started, released sync.Once
	d := newEventDispatcher(kinds, func(ev Event) {
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
	// An idle dispatcher admits any lone record, so hold a selected record's
	// callback: only filtering can then admit an excluded record over budget.
	d, entered, _ := pausedSelectedDispatcher(t, eventBit(ChildOutput), func(Event) {})
	if err := d.emit(Event{Kind: ChildOutput, Bytes: []byte("held")}); err != nil {
		t.Fatal(err)
	}
	<-entered
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
