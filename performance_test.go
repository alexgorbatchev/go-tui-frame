package frame

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/alexgorbatchev/go-tui-frame/v2/internal/emulator"
	"github.com/alexgorbatchev/go-tui-frame/v2/internal/input"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	ghostty "go.mitchellh.com/libghostty"
	"golang.org/x/sys/unix"
)

func TestRepaintDefersCaptureAndSynchronizesInputImmediately(t *testing.T) {
	s, out, slave := newRepaintSession(t, ansi.ModeReset, nil)
	s.console.entry[1] = ansi.ModeReset
	readRepaintChunk(t, s, slave, "a\x1b[?1h")
	if !s.console.applied[1] {
		t.Fatal("input mode waited for a repaint")
	}
	if !bytes.Contains(repaintOutput(t, out), []byte("\x1b[?1h")) {
		t.Fatal("outer terminal input mode was buffered until repaint")
	}
	if s.snapshot.Terminal.Cells[0].Content != " " {
		t.Fatal("PTY read captured the viewport before repaint")
	}
	readRepaintChunk(t, s, slave, "b")
	if err := s.render(false); err != nil {
		t.Fatal(err)
	}
	if s.screen.CellAt(0, 0).Content != "a" || s.screen.CellAt(1, 0).Content != "b" {
		t.Fatal("repaint did not capture the accumulated output")
	}
}

func TestRegionCanvasReusedAndSnapshotsRemainOwned(t *testing.T) {
	var views []uv.Screen
	var snapshots []Snapshot
	s, _, slave := newRepaintSession(t, ansi.ModeReset, func(ctx DrawContext[string]) {
		views = append(views, ctx.View)
		snapshots = append(snapshots, ctx.Term)
		if len(views) == 1 {
			ctx.View.SetCell(5, 0, &uv.Cell{Content: "x", Width: 1})
		}
	})
	readRepaintChunk(t, s, slave, "changed")
	if err := s.frame.InvalidateHeader("next"); err != nil {
		t.Fatal(err)
	}
	if err := s.render(false); err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 || views[0] != views[1] {
		t.Fatal("same-sized region allocated another canvas")
	}
	if s.screen.CellAt(5, 0).Content != " " {
		t.Fatal("reused region canvas retained old drawing")
	}
	if snapshots[0].Terminal.Cells[0].Content != " " || snapshots[1].Terminal.Cells[0].Content != "c" {
		t.Fatal("retained callback snapshot borrowed live display storage")
	}
	for i, snap := range snapshots {
		if &snap.Terminal.Cells[0] != &snap.Terminal.Native.Cells[0] {
			t.Fatalf("callback snapshot %d holds Terminal.Cells and Native.Cells as separate grids", i)
		}
		if &snap.Terminal.Cells[0] == &s.snapshot.Terminal.Cells[0] {
			t.Fatalf("callback snapshot %d shares the session's live cell grid", i)
		}
	}
	snapshots[1].Terminal.Cells[0].Content = "mutated"
	if s.snapshot.Terminal.Cells[0].Content != "c" {
		t.Fatal("callback mutation reached internal display storage")
	}
}

var cloneSink Snapshot

// allocatedBytes reports the heap bytes one call of f allocates, averaged over
// runs, on one P as testing.AllocsPerRun measures allocation counts.
func allocatedBytes(runs int, f func()) uint64 {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	f()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range runs {
		f()
	}
	runtime.ReadMemStats(&after)
	return (after.TotalAlloc - before.TotalAlloc) / uint64(runs)
}

func TestCloneSnapshotCopiesSharedCellGridOnce(t *testing.T) {
	const cells = 120 * 40
	snap := sharedCellSnapshot(cells)
	clone := func() { cloneSink = cloneSnapshot(snap) }
	if allocs := testing.AllocsPerRun(20, clone); allocs != 2 {
		t.Fatalf("cloneSnapshot allocated %.0f times, want 2: one uv.Cell grid and one native cell grid", allocs)
	}
	grid := uint64(cells * unsafe.Sizeof(uv.Cell{}))
	owned := grid + uint64(cells*unsafe.Sizeof(emulator.NativeCell{}))
	// A second uv.Cell grid would add grid bytes; half of it bounds the
	// allocator's size-class rounding of the two owned grids.
	if got := allocatedBytes(20, clone); got >= owned+grid/2 {
		t.Fatalf("cloneSnapshot allocated %d bytes per call, want about %d for one shared cell grid", got, owned)
	}
}

func TestInputQueueReusesDrainedStorage(t *testing.T) {
	s, _, slave := newRepaintSession(t, ansi.ModeReset, nil)
	data := []byte("input")
	received := make([]byte, len(data))
	cycle := func() {
		if err := s.enqueue(data, "user-input"); err != nil {
			t.Fatal(err)
		}
		data[0] = 'X'
		if err := s.writeInput(); err != nil {
			t.Fatal(err)
		}
		if n, err := unix.Read(int(slave.Fd()), received); err != nil || n != len(data) || !bytes.Equal(received, []byte("input")) {
			t.Fatalf("queued input lost ownership/order: %q, %d, %v", received, n, err)
		}
		data[0] = 'i'
	}
	cycle()
	if allocs := testing.AllocsPerRun(100, cycle); allocs != 0 {
		t.Fatalf("warmed input queue allocated %.0f times per delivery", allocs)
	}
}

func TestUnchangedRenderHasNoOutputOrAllocations(t *testing.T) {
	s, out, _ := newRepaintSession(t, ansi.ModeReset, nil)
	before := repaintOutput(t, out)
	if allocs := testing.AllocsPerRun(100, func() {
		if err := s.render(false); err != nil {
			t.Fatal(err)
		}
	}); allocs != 0 {
		t.Fatalf("unchanged frame allocated %.0f times", allocs)
	}
	if !bytes.Equal(before, repaintOutput(t, out)) {
		t.Fatal("unchanged frame produced terminal output")
	}
}

func TestCursorAppearanceRepaintsWithoutCellOrPositionChanges(t *testing.T) {
	s, out, slave := newRepaintSession(t, ansi.ModeReset, nil)
	style := 2
	rgb := ghostty.ColorRGB{R: 0xab, G: 0xcd, B: 0xef}
	s.console.preferences.cursorStyle = &style
	s.console.preferences.profile.Cursor = &rgb
	s.composed = false
	if err := s.render(false); err != nil {
		t.Fatal(err)
	}
	before := len(repaintOutput(t, out))
	for _, step := range []struct {
		sequence, output string
	}{
		{"\x1b[3 q", ansi.SetCursorStyle(3)},
		{"\x1b]12;#123456\a", ansi.SetCursorColor("#123456")},
		{"\x1b]112\a", ansi.ResetCursorColor},
	} {
		readRepaintChunk(t, s, slave, step.sequence)
		if err := s.render(false); err != nil {
			t.Fatal(err)
		}
		output := repaintOutput(t, out)
		paint := output[before:]
		if !bytes.Contains(paint, []byte(step.output)) || bytes.Count(paint, []byte(ansi.SetModeSynchronizedOutput)) != 1 || bytes.Count(paint, []byte(ansi.ResetModeSynchronizedOutput)) != 1 {
			t.Fatalf("appearance-only change %q did not repaint atomically: %q", step.sequence, paint)
		}
		before = len(output)
	}
	if allocs := testing.AllocsPerRun(100, func() {
		if err := s.render(false); err != nil {
			t.Fatal(err)
		}
	}); allocs != 0 {
		t.Fatalf("unchanged inherited cursor allocated %.0f times", allocs)
	}
	if len(repaintOutput(t, out)) != before {
		t.Fatal("unchanged inherited cursor produced output")
	}
}

func TestInputQueueCompactsPartialWritesAndPreservesOrigins(t *testing.T) {
	s, _, slave := newRepaintSession(t, ansi.ModeReset, nil)
	slaveFD := int(slave.Fd())
	if err := unix.SetNonblock(slaveFD, true); err != nil {
		t.Fatal(err)
	}
	first := bytes.Repeat([]byte("abc"), inputQueueLimit/6)
	if err := s.enqueue(first, "user-input"); err != nil {
		t.Fatal(err)
	}
	if err := s.writeInput(); err != nil {
		t.Fatal(err)
	}
	if len(s.queue) != 1 || s.queue[0].offset == 0 || s.queue[0].offset == len(first) {
		t.Fatal("real PTY did not produce a partial write")
	}
	tail := bytes.Repeat([]byte("d"), inputQueueLimit-s.queuedBytes)
	if err := s.enqueue(tail, "user-input"); err != nil {
		t.Fatal(err)
	}
	if len(s.queue[0].bytes) > inputQueueLimit {
		t.Fatal("appending to a partial write retained the consumed prefix beyond the queue budget")
	}
	want := append(first, tail...)
	received := make([]byte, 64<<10)
	var got []byte
	deadline := time.Now().Add(2 * time.Second)
	for len(s.queue) > 0 || len(got) < len(want) {
		if time.Now().After(deadline) {
			t.Fatal("input queue did not drain")
		}
		if err := s.writeInput(); err != nil {
			t.Fatal(err)
		}
		n, err := unix.Read(slaveFD, received)
		if err != nil && err != unix.EAGAIN {
			t.Fatal(err)
		}
		if n > 0 {
			got = append(got, received[:n]...)
		}
	}
	if !bytes.Equal(got, want) {
		t.Fatal("partial write compaction reordered or lost input")
	}
	var events []Event
	s.events = newEventDispatcher(eventBit(ChildInput), sessionCancel(t), func(ev Event) { events = append(events, ev) })
	t.Cleanup(s.events.close)
	for _, origin := range []string{"terminal-reply", "user-input"} {
		if err := s.enqueue([]byte(origin), origin); err != nil {
			t.Fatal(err)
		}
	}
	for len(s.queue) > 0 {
		if err := s.writeInput(); err != nil {
			t.Fatal(err)
		}
	}
	s.events.close()
	if len(events) != 2 || events[0].Origin != "terminal-reply" || string(events[0].Bytes) != "terminal-reply" || events[1].Origin != "user-input" {
		t.Fatal("buffer reuse corrupted origin boundaries or retained observer bytes")
	}
}

func BenchmarkSessionRepaint(b *testing.B) {
	for _, name := range []string{"unchanged render", "unchanged capture", "changing row"} {
		b.Run(name, func(b *testing.B) {
			s, out, _ := newRepaintSession(b, ansi.ModeNotRecognized, nil)
			text := []byte("\x1b[Hupdated")
			before := repaintPosition(b, out)
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				if name == "unchanged render" {
					if err := s.render(false); err != nil {
						b.Fatal(err)
					}
					continue
				}
				if name == "changing row" {
					text[3] = byte('a' + i%2)
					if _, err := s.terminal.Write(text); err != nil {
						b.Fatal(err)
					}
				}
				if err := s.refresh(false); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(repaintPosition(b, out)-before)/float64(b.N), "output-B/op")
		})
	}
}

func repaintPosition(b *testing.B, out io.Seeker) int64 {
	b.Helper()
	pos, err := out.Seek(0, io.SeekCurrent)
	if err != nil {
		b.Fatal(err)
	}
	return pos
}

func BenchmarkPopulatedSessionRepaint(b *testing.B) {
	s, out, _ := newRepaintSessionSize(b, Size{Cols: 120, Rows: 40}, ansi.ModeNotRecognized, nil)
	var initial bytes.Buffer
	for y := range s.geometry.child.Dy() {
		fmt.Fprintf(&initial, "\x1b[%d;1H%s", y+1, strings.Repeat("x", s.geometry.child.Dx()-1))
	}
	if _, err := s.terminal.Write(initial.Bytes()); err != nil {
		b.Fatal(err)
	}
	if err := s.refresh(false); err != nil {
		b.Fatal(err)
	}
	before := repaintPosition(b, out)
	text := []byte("\x1b[Hupdated")
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		text[3] = byte('a' + i%2)
		if _, err := s.terminal.Write(text); err != nil {
			b.Fatal(err)
		}
		if err := s.refresh(false); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(repaintPosition(b, out)-before)/float64(b.N), "output-B/op")
}

func BenchmarkSessionInputQueue(b *testing.B) {
	s, _, slave := newRepaintSession(b, ansi.ModeNotRecognized, nil)
	data := []byte("input")
	buf := make([]byte, len(data))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := s.enqueue(data, "user-input"); err != nil {
			b.Fatal(err)
		}
		if err := s.writeInput(); err != nil {
			b.Fatal(err)
		}
		if n, err := unix.Read(int(slave.Fd()), buf); err != nil || n != len(data) {
			b.Fatalf("input delivery: %d, %v", n, err)
		}
	}
}

func TestCompositionTouchesOnlyDamagedAreas(t *testing.T) {
	f := New(exec.Command("sh"), "header").Header(1, func(ctx DrawContext[string]) {
		ctx.View.SetCell(0, 0, &uv.Cell{Content: "H", Width: 1})
	}).Border(true)
	g, err := f.layout(Size{Cols: 10, Rows: 7})
	if err != nil {
		t.Fatal(err)
	}
	buf := uv.NewScreenBuffer(10, 7)
	cells := make([]uv.Cell, g.child.Dx()*g.child.Dy())
	for i := range cells {
		cells[i] = uv.Cell{Content: "a", Width: 1}
	}
	snap := Snapshot{Terminal: TerminalSnapshot{Cells: cells, Size: Size{Cols: g.child.Dx(), Rows: g.child.Dy()}}}
	f.compose(&buf, g, snap, repaintDamage{full: true})
	clear(buf.Touched)
	cells[g.child.Dx()].Content = "b"
	rows := make([]bool, g.child.Dy())
	rows[1] = true
	f.compose(&buf, g, snap, repaintDamage{rows: rows})
	if buf.TouchedLines() != 1 || buf.Touched[g.child.Min.Y+1] == nil {
		t.Fatalf("single-row update touched %d outer rows", buf.TouchedLines())
	}
	if buf.CellAt(0, 0).Content != "H" || buf.CellAt(g.border.Min.X, g.border.Min.Y).Content != borderTopLeft {
		t.Fatal("incremental composition damaged unchanged chrome")
	}
}

func TestObservationSelectionPreservesPerReadSnapshotsAndDamage(t *testing.T) {
	for _, tt := range []struct {
		name    string
		kinds   []EventKind
		capture bool
	}{{"raw output", []EventKind{ChildOutput}, false}, {"state", []EventKind{StateChanged}, true}, {"all", nil, true}} {
		t.Run(tt.name, func(t *testing.T) {
			s, _, slave := newRepaintSession(t, ansi.ModeReset, nil)
			var got []Event
			handler := func(ev Event) { got = append(got, ev) }
			if tt.kinds == nil {
				s.frame.Observe(handler)
			} else {
				s.frame.ObserveEvents(tt.kinds, handler)
				tt.kinds[0] = OuterInput // Configuration owns the subscription.
			}
			s.events = newEventDispatcher(s.frame.observedKinds, sessionCancel(t), s.frame.observe)
			t.Cleanup(s.events.close)
			readRepaintChunk(t, s, slave, "one")
			if captured := s.snapshot.Terminal.Cells[0].Content == "o"; captured != tt.capture {
				t.Fatalf("viewport capture = %v, want %v", captured, tt.capture)
			}
			readRepaintChunk(t, s, slave, "\x1b[2;1Htwo")
			if err := s.render(false); err != nil {
				t.Fatal(err)
			}
			s.events.close()
			if s.screen.CellAt(0, 0).Content != "o" || s.screen.CellAt(0, 1).Content != "t" {
				t.Fatal("capture for observations consumed pending repaint damage")
			}
			var states []*Snapshot
			var raw []Event
			for _, ev := range got {
				if ev.Kind == StateChanged {
					states = append(states, ev.Snapshot)
				}
				if ev.Kind == ChildOutput {
					raw = append(raw, ev)
				}
			}
			if tt.capture {
				if len(states) != 2 || states[0].Terminal.Cells[20].Content != " " || states[1].Terminal.Cells[20].Content != "t" {
					t.Fatal("state subscription lost per-read immutable snapshots")
				}
			} else if len(states) != 0 || len(raw) != 2 || raw[0].Offset != 0 || raw[1].Offset != 3 {
				t.Fatal("raw-only subscription received state or incorrect offsets")
			}
			for i, ev := range got {
				if ev.Sequence != uint64(i+1) {
					t.Fatal("selected event sequence has gaps")
				}
			}
		})
	}
}

// The session loop reaps the launch leader while it runs, and cleanup once it
// has returned. After Started, either reap emits one Exited carrying the
// display the child's last output left, also when Wait fails without reporting
// an exit status or the loop cannot route held input. Cleanup for a session
// that failed before Started emits none.
func TestExitSubscriptionCapturesPendingOutput(t *testing.T) {
	loopReap := func(s *session[string], waitErr error) error {
		s.wait = make(chan error, 1)
		s.wait <- waitErr
		return s.reap()
	}
	// cleanupChild emits Exited once finishWait has recorded a leader it reaped.
	cleanupReap := func(s *session[string], waitErr error) error {
		return errors.Join(s.finishWait(waitErr), s.emitExited())
	}
	waitFailure := errors.New("wait failed")
	for _, tt := range []struct {
		name    string
		reap    func(s *session[string], waitErr error) error
		started bool
		waitErr error
		// unroutable holds input for the reap to route behind a border that
		// cannot fit, so routing it fails with ErrViewportTooSmall.
		unroutable bool
		want       error
	}{
		{name: "session loop", reap: loopReap, started: true},
		{name: "session loop after Wait failure", reap: loopReap, started: true, waitErr: waitFailure, want: waitFailure},
		{name: "session loop routing held input fails", reap: loopReap, started: true, unroutable: true, want: ErrViewportTooSmall},
		{name: "cleanup", reap: cleanupReap, started: true},
		{name: "cleanup after Wait failure", reap: cleanupReap, started: true, waitErr: waitFailure, want: waitFailure},
		{name: "cleanup before Started", reap: cleanupReap},
	} {
		t.Run(tt.name, func(t *testing.T) {
			size := Size{Cols: 20, Rows: 6}
			var header func(DrawContext[string])
			if tt.unroutable {
				// A one-row header leaves the child two of three rows, too few for
				// a border.
				size, header = Size{Cols: 20, Rows: 3}, func(DrawContext[string]) {}
			}
			s, _, slave := newRepaintSessionSize(t, size, ansi.ModeReset, header)
			s.started = tt.started
			var got []Event
			s.events = newEventDispatcher(eventBit(Exited), sessionCancel(t), func(ev Event) { got = append(got, ev) })
			t.Cleanup(s.events.close)
			readRepaintChunk(t, s, slave, "final")
			if tt.unroutable {
				if err := s.frame.SetBorder(true); err != nil {
					t.Fatal(err)
				}
				s.held = []input.Packet{{Raw: []byte("a")}}
			}
			if err := tt.reap(s, tt.waitErr); !errors.Is(err, tt.want) || !s.waited {
				t.Fatalf("reap = %v, waited %v; want %v and the leader reaped", err, s.waited, tt.want)
			}
			s.events.close()
			want := []EventKind{Exited}
			if !tt.started {
				want = nil
			}
			if kinds := eventKinds(got); !slices.Equal(kinds, want) {
				t.Fatalf("exit-only subscriber received %v, want %v", kinds, want)
			}
			if tt.started && got[0].Snapshot.Terminal.Cells[0].Content != "f" {
				t.Fatal("exit-only subscriber received stale display state")
			}
		})
	}
}

// When the loop reaps the launch leader behind an observer that has fallen
// behind, routing held input and emitting Exited both meet the full queue.
// reap reports that one overflow once.
func TestReapReportsAFullObservationQueueOnce(t *testing.T) {
	s, _, _ := newRepaintSession(t, ansi.ModeReset, nil)
	s.started = true
	router, err := newInputRouter(s.terminal, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(router.close)
	s.router = router
	d, entered, _ := pausedSelectedDispatcher(t, eventBit(Routed)|eventBit(Exited), sessionCancel(t), func(Event) {})
	s.events = d
	// The first record's callback holds while records fill the queue behind it.
	if err := d.emit(Event{Kind: Routed}); err != nil {
		t.Fatal(err)
	}
	<-entered
	for range observationQueueLimit {
		if err := d.emit(Event{Kind: Routed}); err != nil {
			t.Fatal(err)
		}
	}
	// The child has not requested focus reports, so routing drops this one and
	// emits Routed.
	s.held = []input.Packet{{Raw: []byte("\x1b[I"), Event: uv.FocusEvent{}}}
	s.wait = make(chan error, 1)
	s.wait <- nil
	err = s.reap()
	if !errors.Is(err, ErrObservationOverflow) {
		t.Fatalf("reap = %v, want %v", err, ErrObservationOverflow)
	}
	if n := strings.Count(err.Error(), ErrObservationOverflow.Error()); n != 1 {
		t.Fatalf("reap reported the overflow %d times, want once: %v", n, err)
	}
}

func TestDeferredCapturePreservesSynchronizedCheckpoint(t *testing.T) {
	for _, release := range []string{"child", "deadline"} {
		t.Run(release, func(t *testing.T) {
			s, _, slave := newRepaintSession(t, ansi.ModeReset, nil)
			readRepaintChunk(t, s, slave, "complete")
			readRepaintChunk(t, s, slave, "\x1b[?2026h\rpartial")
			if err := s.render(false); err != nil {
				t.Fatal(err)
			}
			if s.screen.CellAt(0, 0).Content != "c" || s.holdDeadline.IsZero() {
				t.Fatal("deferred capture lost the complete frame before a hold")
			}
			if release == "child" {
				readRepaintChunk(t, s, slave, "\x1b[?2026l")
			} else {
				s.holdDeadline = time.Now().Add(-time.Second)
				if err := s.deadlines(); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.render(false); err != nil {
				t.Fatal(err)
			}
			if s.screen.CellAt(0, 0).Content != "p" || !s.holdDeadline.IsZero() {
				t.Fatal("released hold did not paint pending rows")
			}
		})
	}
}

func TestUnobservedRoutingAndEffectsDoNotAllocate(t *testing.T) {
	s, _, _ := newRepaintSession(t, ansi.ModeReset, nil)
	s.started = true
	router, err := newInputRouter(s.terminal, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(router.close)
	s.router = router
	s.router.filter.handler = func(Input) Disposition { return Consume }

	packet := input.Packet{
		Raw:   []byte("\x03"),
		Event: uv.KeyPressEvent{Code: 'c', Mod: uv.ModCtrl},
	}

	// 1. Verify consumed key routing allocates only for the handler's Input.Raw
	// copy, and does not heap-allocate an Input or Event when Captured is unobserved.
	if allocs := testing.AllocsPerRun(100, func() {
		if err := s.route(packet); err != nil {
			t.Fatal(err)
		}
	}); allocs != 1 {
		t.Fatalf("unobserved consumed key allocated %.0f times, want 1", allocs)
	}

	// 2. Verify effects() does not heap-allocate an Effect when Protocol is unobserved.
	// Write generates a CPR reply effect in libghostty; s.effects() must process
	// that effect without adding any heap allocation over Write itself.
	writeAllocs := testing.AllocsPerRun(100, func() {
		if _, err := s.terminal.Write([]byte("\a")); err != nil {
			t.Fatal(err)
		}
		_ = s.terminal.Effects()
	})

	totalAllocs := testing.AllocsPerRun(100, func() {
		if _, err := s.terminal.Write([]byte("\a")); err != nil {
			t.Fatal(err)
		}
		if err := s.effects(); err != nil {
			t.Fatal(err)
		}
	})

	if totalAllocs > writeAllocs {
		t.Fatalf("unobserved effects() added %.0f allocations (total %.0f > write %.0f), want 0", totalAllocs-writeAllocs, totalAllocs, writeAllocs)
	}
}
