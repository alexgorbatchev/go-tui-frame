package frame

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/alexgorbatchev/go-tui-frame/v2/internal/emulator"
	uv "github.com/charmbracelet/ultraviolet"
	ghostty "go.mitchellh.com/libghostty"
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
		// An observed error is retained with its record, so its text weighs
		// against the budget like the record's bytes do.
		{"executing callback rejects record whose error fills the budget", &Event{Kind: Started}, Event{Kind: Routed, Error: errors.New(strings.Repeat("x", observationByteLimit))}, ErrObservationOverflow},
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

// sharedCellSnapshot returns a snapshot shaped like the session's live one:
// Terminal.Cells and Terminal.Native.Cells are the same cell grid.
func sharedCellSnapshot(cells int) Snapshot {
	native := NativeState{Cells: make([]uv.Cell, cells), NativeCells: make([]emulator.NativeCell, cells)}
	return Snapshot{Terminal: TerminalSnapshot{Cells: native.Cells, Native: native}}
}

func TestObservedSnapshotsShareOneOwnedCellGrid(t *testing.T) {
	var got []Event
	d, entered, release := pausedDispatcher(t, func(ev Event) { got = append(got, ev) })
	live := sharedCellSnapshot(2)
	live.Terminal.Cells[0] = uv.Cell{Content: "a", Width: 1}
	if err := d.emit(Event{Kind: StateChanged, Snapshot: &live}); err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := d.emit(Event{Kind: StateChanged, Snapshot: &live}); err != nil {
		t.Fatal(err)
	}
	live.Terminal.Cells[0].Content = "live"
	release()
	d.close()
	if len(got) != 2 {
		t.Fatalf("observed %d snapshots, want 2", len(got))
	}
	for i, ev := range got {
		term := ev.Snapshot.Terminal
		if &term.Cells[0] != &term.Native.Cells[0] {
			t.Fatalf("snapshot %d holds Terminal.Cells and Native.Cells as separate grids", i)
		}
		if &term.Cells[0] == &live.Terminal.Cells[0] || term.Cells[0].Content != "a" {
			t.Fatalf("snapshot %d borrowed live cell storage: %q", i, term.Cells[0].Content)
		}
	}
	got[0].Snapshot.Terminal.Native.Cells[0].Content = "observer"
	if got[0].Snapshot.Terminal.Cells[0].Content != "observer" {
		t.Fatal("a native cell edit did not reach the shared Terminal.Cells grid")
	}
	if got[1].Snapshot.Terminal.Cells[0].Content != "a" || live.Terminal.Cells[0].Content != "live" {
		t.Fatal("an observer's cell edit reached another snapshot or live storage")
	}
}

func TestEventWeightCountsSharedCellGridOnce(t *testing.T) {
	shared := sharedCellSnapshot(4)
	shared.Terminal.Cells[0] = uv.Cell{Content: "grid", Width: 1, Link: uv.Link{URL: "https://example.com", Params: "id=1"}}
	nativeOnly := shared
	nativeOnly.Terminal.Cells = nil
	separate := shared
	separate.Terminal.Cells = slices.Clone(shared.Terminal.Cells)
	grid := 0
	for _, c := range shared.Terminal.Cells {
		grid += int(unsafe.Sizeof(c)) + len(c.Content) + len(c.Link.URL) + len(c.Link.Params)
	}
	base := eventWeight(Event{Kind: StateChanged, Snapshot: &nativeOnly})
	for _, tt := range []struct {
		name string
		snap Snapshot
		want int
	}{
		{"shared grid", shared, base},
		{"separate grid", separate, base + grid},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := eventWeight(Event{Kind: StateChanged, Snapshot: &tt.snap}); got != tt.want {
				t.Fatalf("eventWeight = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestEventWeightCountsNativeStyleTable(t *testing.T) {
	const styles = 5
	plain := sharedCellSnapshot(4)
	styled := plain
	styled.Terminal.Native.NativeStyles = make([]ghostty.Style, styles)
	want := eventWeight(Event{Kind: StateChanged, Snapshot: &plain}) + styles*int(unsafe.Sizeof(ghostty.Style{}))
	if got := eventWeight(Event{Kind: StateChanged, Snapshot: &styled}); got != want {
		t.Fatalf("eventWeight with a %d-style table = %d, want %d", styles, got, want)
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
	done := startRun(ctx, app)
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

// A Started that overflows the queue fails Run before the session has started
// the child's lifecycle, so the reap in cleanup emits no Exited, even for an
// observer that catches up before that reap.
func TestOverflowingStartedIsFollowedByNoExited(t *testing.T) {
	// One Started snapshot of this terminal outweighs the whole budget, so it
	// overflows while any other record's callback runs.
	h := newSizedHarness(t, 512, 256)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	blocked, resume := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	t.Cleanup(release)
	var first sync.Once
	var observed eventLog
	app := New(childCommand(t), struct{}{}).Terminal(h.slave, h.slave)
	app.ObserveEvents([]EventKind{OuterInput, Started, Exited}, func(e Event) {
		observed.record(e)
		// The first record is a startup probe reply; holding its callback
		// keeps the budget occupied when Started arrives.
		first.Do(func() { close(blocked); <-resume })
	})
	done := startRun(ctx, app)
	select {
	case <-blocked:
	case <-ctx.Done():
		t.Fatal("startup probe input was not observed", ctx.Err())
	}
	// The controller closes as Run's body returns, before cleanup reaps the
	// child. Releasing the observer then leaves the dispatcher idle, so any
	// Exited the reap emitted would be admitted and delivered.
	for !errors.Is(app.SetBorder(false), ErrSessionClosed) {
		if ctx.Err() != nil {
			t.Fatal("Run did not end after Started overflowed", ctx.Err())
		}
		time.Sleep(100 * time.Microsecond)
	}
	release()
	var got runOutcome
	select {
	case got = <-done:
	case <-ctx.Done():
		t.Fatal("Run did not finish", ctx.Err())
	}
	if !errors.Is(got.err, ErrObservationOverflow) || got.result.ProcessState == nil || got.result.CleanupError != nil {
		t.Fatalf("Run = %#v, %v; want %v, the reaped child's state and no cleanup error", got.result, got.err, ErrObservationOverflow)
	}
	if kinds := eventKinds(observed.events()); slices.Contains(kinds, Started) || slices.Contains(kinds, Exited) {
		t.Fatalf("observer received %v, want neither Started nor Exited", kinds)
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
	return pausedSelectedDispatcher(t, allEvents, sessionCancel(t), handler)
}

// pausedSelectedDispatcher observes kinds and holds the first delivered
// record's callback until release, so later records meet a non-idle budget.
// A callback panic or runtime.Goexit cancels the session through fail.
func pausedSelectedDispatcher(t *testing.T, kinds eventMask, fail context.CancelCauseFunc, handler func(Event)) (*eventDispatcher, <-chan struct{}, func()) {
	t.Helper()
	entered, resume := make(chan struct{}), make(chan struct{})
	var started, released sync.Once
	d := newEventDispatcher(kinds, fail, func(ev Event) {
		started.Do(func() { close(entered); <-resume })
		handler(ev)
	})
	release := func() { released.Do(func() { close(resume) }) }
	t.Cleanup(func() { release(); d.close() })
	return d, entered, release
}

// sessionCancel returns the session cancellation a dispatcher reports a
// callback panic or runtime.Goexit through, for tests whose handlers must
// return normally. The dispatcher would otherwise swallow such a failure, so a
// recorded cause fails the test. Call it before registering the dispatcher's
// close as a cleanup: cleanups run in reverse order, so the check then follows
// the final drain.
func sessionCancel(t *testing.T) context.CancelCauseFunc {
	t.Helper()
	// t.Context is canceled before cleanups run, which would set a cause here.
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() {
		if ctx.Err() != nil {
			t.Errorf("observer callback failed: %v", context.Cause(ctx))
		}
		cancel(nil)
	})
	return cancel
}

func TestObserverFailureCancelsSessionAndDiscardsLaterRecords(t *testing.T) {
	// The failing callbacks are closures of this test, so its name appears in
	// the stack the dispatcher reports.
	test := t.Name()
	var empty []int
	for _, tt := range []struct {
		name string
		// godebug, when set, replaces GODEBUG for the subtest. The runtime
		// rereads its panicnil setting whenever GODEBUG changes.
		godebug string
		fault   func(Event)
		want    func(error) bool
	}{
		{"value", "", func(Event) { panic("observer fault") }, func(err error) bool {
			return strings.Contains(err.Error(), "observer fault")
		}},
		{"runtime error", "", func(ev Event) { _ = empty[len(ev.Bytes)] }, func(err error) bool {
			var runtimeErr runtime.Error
			return errors.As(err, &runtimeErr) && strings.Contains(err.Error(), "index out of range")
		}},
		{"nil with panicnil=0", "panicnil=0", func(Event) { panic(nil) }, func(err error) bool {
			var nilErr *runtime.PanicNilError
			return errors.As(err, &nilErr)
		}},
		// recover returns nil for this panic, as it does when nothing panicked.
		{"nil with panicnil=1", "panicnil=1", func(Event) { panic(nil) }, func(err error) bool {
			return strings.HasPrefix(err.Error(), "observer panicked: <nil>\n")
		}},
		// No recover stops runtime.Goexit, so the callback's goroutine ends.
		{"runtime.Goexit", "", func(Event) { runtime.Goexit() }, func(err error) bool {
			return errors.Is(err, errObserverGoexit)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.godebug != "" {
				t.Setenv("GODEBUG", tt.godebug)
			}
			ctx, cancel := context.WithCancelCause(t.Context())
			calls := 0
			d, entered, release := pausedSelectedDispatcher(t, allEvents, cancel, func(ev Event) {
				calls++
				tt.fault(ev)
			})
			if err := d.emit(Event{Kind: Started}); err != nil {
				t.Fatal(err)
			}
			<-entered
			for _, data := range []string{"queued", "behind"} {
				if err := d.emit(Event{Kind: ChildOutput, Bytes: []byte(data)}); err != nil {
					t.Fatalf("queue rejected a record behind the executing callback: %v", err)
				}
			}
			release()
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("observer failure did not cancel the session")
			}
			cause := context.Cause(ctx)
			if !tt.want(cause) || !strings.Contains(cause.Error(), test) {
				t.Fatalf("session cause = %v, want the failure and the callback's stack", cause)
			}
			snap := Snapshot{Terminal: TerminalSnapshot{Cells: []uv.Cell{{Content: "state", Width: 1}}}}
			// Over the queue limit: a record still copied and queued would overflow.
			if allocs := testing.AllocsPerRun(observationQueueLimit*2, func() {
				if err := d.emit(Event{Kind: StateChanged, Snapshot: &snap}); err != nil {
					t.Fatal(err)
				}
			}); allocs != 0 {
				t.Fatalf("record emitted after the failure allocated %.0f times", allocs)
			}
			d.close()
			if calls != 1 {
				t.Fatalf("callback ran %d times, want only the call that failed", calls)
			}
			if d.bytes != 0 {
				t.Fatalf("drained dispatcher holds %d bytes", d.bytes)
			}
			if d.failure() != cause {
				t.Fatalf("dispatcher failure = %v, want the session cause", d.failure())
			}
		})
	}
}

func TestDisabledObservationDoesNotRetainOrDeliver(t *testing.T) {
	d := newEventDispatcher(allEvents, sessionCancel(t), nil)
	if d != nil {
		t.Fatal("disabled observer allocated dispatcher")
	}
	if err := d.emit(Event{Kind: Started}); err != nil {
		t.Fatal(err)
	}
	d.close()
}

func TestObserverMutationDoesNotChangeQueueAccounting(t *testing.T) {
	d := newEventDispatcher(allEvents, sessionCancel(t), func(ev Event) {
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
			d := newEventDispatcher(f.observedKinds, sessionCancel(t), f.observe)
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
	d, entered, _ := pausedSelectedDispatcher(t, eventBit(ChildOutput), sessionCancel(t), func(Event) {})
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
