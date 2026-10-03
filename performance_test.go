package frame

import (
	"bytes"
	"os/exec"
	"testing"
	"time"

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
	snapshots[1].Terminal.Cells[0].Content = "mutated"
	if s.snapshot.Terminal.Cells[0].Content != "c" {
		t.Fatal("callback mutation reached internal display storage")
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
		{"\x1b]112\a", ansi.SetCursorColor("#abcdef")},
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
	if err := unix.SetNonblock(int(slave.Fd()), true); err != nil {
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
		n, err := unix.Read(int(slave.Fd()), received)
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
	s.events = newEventDispatcher(eventBit(ChildInput), func(ev Event) { events = append(events, ev) })
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
			s, _, _ := newRepaintSession(b, ansi.ModeNotRecognized, nil)
			text := []byte("\x1b[Hupdated")
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
		})
	}
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
			s.events = newEventDispatcher(s.frame.observedKinds, s.frame.observe)
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

func TestExitSubscriptionCapturesPendingOutput(t *testing.T) {
	s, _, slave := newRepaintSession(t, ansi.ModeReset, nil)
	var got Event
	s.events = newEventDispatcher(eventBit(Exited), func(ev Event) { got = ev })
	t.Cleanup(s.events.close)
	readRepaintChunk(t, s, slave, "final")
	s.wait = make(chan error, 1)
	s.wait <- nil
	if err := s.reap(); err != nil {
		t.Fatal(err)
	}
	s.events.close()
	if got.Kind != Exited || got.Snapshot.Terminal.Cells[0].Content != "f" {
		t.Fatal("exit-only subscriber received stale display state")
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
