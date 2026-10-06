package frame

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/alexgorbatchev/go-tui-frame/internal/emulator"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"github.com/creack/pty"
	ghostty "go.mitchellh.com/libghostty"
	"golang.org/x/sys/unix"
)

// The child runs in a separate real PTY and uses the slave's native line discipline.
func TestSessionChild(t *testing.T) {
	if os.Getenv("FRAME_TEST_CHILD") != "1" {
		return
	}
	if os.Getenv("FRAME_TEST_MODE") == "sleep" {
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	if os.Getenv("FRAME_TEST_MODE") == "count" {
		countInput(os.Getenv("FRAME_TEST_COUNT_FILE"))
	}
	if os.Getenv("FRAME_TEST_MODE") == "groups" {
		// Kitty disambiguation is a keyboard mode the frame mirrors to the outer
		// terminal, where only restoration disables it.
		if _, err := fmt.Fprint(os.Stdout, ansi.PushKittyKeyboard(int(ghostty.KittyKeyDisambiguate))); err != nil {
			os.Exit(92)
		}
		sleeper := startSleeper(&syscall.SysProcAttr{Setpgid: true})
		if err := sleeper.Wait(); err != nil {
			os.Exit(97)
		}
		os.Exit(0)
	}
	if mode := os.Getenv("FRAME_TEST_MODE"); mode == "detached" || mode == "detached-on-input" {
		// The sleeper keeps the slave open from its own session, as a daemon
		// the framed program starts with setsid would; cleanup never signals it.
		startSleeper(&syscall.SysProcAttr{Setsid: true})
		if mode == "detached-on-input" {
			awaitFloodInput()
		}
		floodOutput()
	}
	if _, err := term.MakeRaw(os.Stdin.Fd()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(91)
	}
	if _, err := fmt.Fprint(os.Stdout, "\x1b[2J\x1b[Hchild\x1b]2;Child title\x07\x1b[6n"); err != nil {
		os.Exit(92)
	}
	buf := make([]byte, 256)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			os.Exit(93)
		}
		if strings.Contains(string(buf[:n]), "q") {
			if _, err := fmt.Fprint(os.Stdout, "\x1b[2;1Hfinished"); err != nil {
				os.Exit(94)
			}
			os.Exit(0)
		}
	}
}

// startSleeper starts a sleeping child with attrs on this process's terminal
// and publishes its PID in FRAME_TEST_PID_FILE.
func startSleeper(attrs *syscall.SysProcAttr) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestSessionChild$")
	cmd.Env = append(os.Environ(), "FRAME_TEST_MODE=sleep")
	cmd.SysProcAttr = attrs
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		os.Exit(95)
	}
	pidFile := os.Getenv("FRAME_TEST_PID_FILE")
	if err := os.WriteFile(pidFile+".pending", []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
		os.Exit(96)
	}
	if err := os.Rename(pidFile+".pending", pidFile); err != nil {
		os.Exit(98)
	}
	return cmd
}

// awaitFloodInput writes one short output and waits for a byte of input, so a
// test can start the flood once it has observed that output.
func awaitFloodInput() {
	if _, err := term.MakeRaw(os.Stdin.Fd()); err != nil {
		os.Exit(91)
	}
	if _, err := fmt.Fprint(os.Stdout, "ready"); err != nil {
		os.Exit(92)
	}
	if _, err := os.Stdin.Read(make([]byte, 1)); err != nil {
		os.Exit(93)
	}
}

// floodOutput writes to the terminal until a write fails or the process is
// killed, so output stays queued on the PTY whenever the frame stops reading.
func floodOutput() {
	chunk := bytes.Repeat([]byte("flood "), 1024)
	for {
		if _, err := os.Stdout.Write(chunk); err != nil {
			os.Exit(99)
		}
	}
}

// countInput runs a child that sends the terminal no queries: it counts the
// input bytes before "q", records the count in file and exits.
func countInput(file string) {
	if _, err := term.MakeRaw(os.Stdin.Fd()); err != nil {
		os.Exit(91)
	}
	if _, err := fmt.Fprint(os.Stdout, "counting"); err != nil {
		os.Exit(92)
	}
	total, buf := 0, make([]byte, 65536)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			os.Exit(93)
		}
		if i := bytes.IndexByte(buf[:n], 'q'); i >= 0 {
			if err := os.WriteFile(file, []byte(strconv.Itoa(total+i)), 0600); err != nil {
				os.Exit(94)
			}
			os.Exit(0)
		}
		total += n
	}
}

type terminalHarness struct {
	master, slave *os.File
	fd            int
	mu            sync.Mutex
	em            *emulator.Terminal
	stop, done    chan struct{}
	err           error
	// lead is input sent before the first query replies. While a writer
	// sends it, the read loop keeps reading and defers its replies.
	lead, deferred []byte
	leading        bool
	writers        sync.WaitGroup
	// answers maps queries the native emulator ignores to their replies; carry
	// keeps the output tail where a query may continue in the next read.
	answers map[string]string
	carry   []byte
	// replacements change the native emulator's replies.
	replacements []replyReplacement
	// transcript records what the terminal receives once transcribe is called.
	transcript *bytes.Buffer
}

// replyReplacement is the reply a harness terminal sends in place of each
// native reply that native matches.
type replyReplacement struct {
	native *regexp.Regexp
	reply  []byte
}

const harnessCellWidth, harnessCellHeight = 10, 20

func newHarness(t *testing.T) *terminalHarness {
	t.Helper()
	return newSizedHarness(t, 40, 12)
}

// newSizedHarness models an outer terminal of the given grid whose cells
// measure harnessCellWidth x harnessCellHeight pixels.
func newSizedHarness(t *testing.T, cols, rows uint16) *terminalHarness {
	t.Helper()
	return newCellHarness(t, cols, rows, harnessCellWidth, harnessCellHeight)
}

// newCellHarness models an outer terminal of the given grid and cell pixels
// with libghostty's defaults.
func newCellHarness(t *testing.T, cols, rows, cellWidth, cellHeight uint16) *terminalHarness {
	t.Helper()
	return newTerminalHarness(t, harnessTerminal{cols: cols, rows: rows, cellWidth: cellWidth, cellHeight: cellHeight})
}

// harnessTerminal describes the outer terminal a harness models. defaults
// holds its configured defaults, as a Ghostty configuration sets the cursor
// color and style that programs can override and reset; nil keeps libghostty's.
type harnessTerminal struct {
	cols, rows, cellWidth, cellHeight uint16
	defaults                          *emulator.Profile
	// wcwidth starts the terminal measuring text with wcwidth, grapheme
	// clustering (mode 2027) reset, instead of by grapheme clusters.
	wcwidth bool
}

// newTerminalHarness models an outer terminal, keeping the PTY pixel size and
// the emulator answering size queries in agreement. Zero cell pixels model a
// terminal that cannot measure its cells.
func newTerminalHarness(t *testing.T, terminal harnessTerminal) *terminalHarness {
	t.Helper()
	cols, rows, cellWidth, cellHeight := terminal.cols, terminal.rows, terminal.cellWidth, terminal.cellHeight
	m, s, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := pty.Setsize(s, &pty.Winsize{Cols: cols, Rows: rows, X: cols * cellWidth, Y: rows * cellHeight}); err != nil {
		t.Fatal(err)
	}
	em, err := emulator.New(emulator.Options{Size: emulator.Size{Cols: int(cols), Rows: int(rows), CellWidthPx: uint32(cellWidth), CellHeightPx: uint32(cellHeight)}, GraphemeWidth: !terminal.wcwidth, Profile: terminal.defaults})
	if err != nil {
		t.Fatal(err)
	}
	h := &terminalHarness{master: m, slave: s, fd: int(m.Fd()), em: em, stop: make(chan struct{}), done: make(chan struct{})}
	if err := unix.SetNonblock(h.fd, true); err != nil {
		t.Fatal(err)
	}
	go h.read()
	t.Cleanup(func() {
		close(h.stop)
		<-h.done
		h.writers.Wait()
		h.mu.Lock()
		err := h.err
		h.em.Close()
		h.mu.Unlock()
		if err != nil {
			t.Errorf("outer terminal: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Error(err)
		}
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	})
	return h
}

func (h *terminalHarness) read() {
	defer close(h.done)
	buf := make([]byte, 65536)
	for {
		select {
		case <-h.stop:
			return
		default:
		}
		fds := []unix.PollFd{{Fd: int32(h.fd), Events: unix.POLLIN}}
		if _, err := unix.Poll(fds, 20); err != nil && !errors.Is(err, unix.EINTR) {
			h.fail(err)
			return
		}
		if fds[0].Revents&unix.POLLIN == 0 {
			continue
		}
		n, err := unix.Read(h.fd, buf)
		if errors.Is(err, unix.EAGAIN) {
			continue
		}
		if err != nil {
			h.fail(err)
			return
		}
		h.mu.Lock()
		if h.transcript != nil {
			h.transcript.Write(buf[:n])
		}
		_, err = h.em.Write(buf[:n])
		var replies []byte
		for _, e := range h.em.Effects() {
			if e.Kind == emulator.Reply {
				replies = append(replies, e.Bytes...)
			}
		}
		for _, r := range h.replacements {
			replies = r.native.ReplaceAllLiteral(replies, r.reply)
		}
		replies = append(replies, h.answerQueries(buf[:n])...)
		switch {
		case len(replies) > 0 && h.lead != nil:
			h.deferred, replies, h.leading = replies, nil, true
			h.writers.Add(1)
			go h.writeLead(h.lead)
			h.lead = nil
		case h.leading:
			h.deferred, replies = append(h.deferred, replies...), nil
		}
		h.mu.Unlock()
		if err != nil {
			h.fail(err)
			return
		}
		if len(replies) > 0 {
			if err := h.writeReplies(replies); err != nil {
				h.fail(err)
				return
			}
		}
	}
}

// precede makes the terminal send input before it answers its first query, as
// when a user types or pastes while the frame starts.
func (h *terminalHarness) precede(input []byte) {
	h.mu.Lock()
	h.lead = input
	h.mu.Unlock()
}

// writeLead sends the lead input, then the replies deferred meanwhile, as one
// terminal writing in order while it keeps reading the frame's output.
func (h *terminalHarness) writeLead(lead []byte) {
	defer h.writers.Done()
	if err := h.writeReplies(lead); err != nil {
		h.fail(err)
		return
	}
	for {
		h.mu.Lock()
		replies := h.deferred
		h.deferred, h.leading = nil, len(replies) > 0
		h.mu.Unlock()
		if len(replies) == 0 {
			return
		}
		if err := h.writeReplies(replies); err != nil {
			h.fail(err)
			return
		}
	}
}

// answer makes the terminal reply to a query its native emulator ignores.
func (h *terminalHarness) answer(query, reply string) {
	h.mu.Lock()
	if h.answers == nil {
		h.answers = make(map[string]string)
	}
	h.answers[query] = reply
	h.mu.Unlock()
}

// answerQueries returns the configured replies to queries in output, once
// each, including a query split across reads. The caller holds h.mu.
func (h *terminalHarness) answerQueries(output []byte) []byte {
	if len(h.answers) == 0 {
		return nil
	}
	window := append(h.carry, output...)
	var replies []byte
	longest := 0
	for query, reply := range h.answers {
		longest = max(longest, len(query))
		if bytes.Contains(window, []byte(query)) {
			replies = append(replies, reply...)
			delete(h.answers, query)
		}
	}
	h.carry = slices.Clone(window[max(0, len(window)-longest+1):])
	return replies
}

// report makes the terminal answer every DECRQM query for m with reply
// instead of its native report, as a terminal that reports m differently
// does. An empty reply models a terminal that does not answer.
func (h *terminalHarness) report(m ansi.DECMode, reply string) {
	native := regexp.MustCompile(regexp.QuoteMeta(fmt.Sprintf("\x1b[?%d;", m)) + `\d+` + regexp.QuoteMeta("$y"))
	h.replace(native, reply)
}

// withholdKittyKeyboard makes the terminal leave every Kitty keyboard query
// (CSI ? u) unanswered, as a terminal without the protocol does.
func (h *terminalHarness) withholdKittyKeyboard() {
	h.replace(regexp.MustCompile(`\x1b\[\?\d+u`), "")
}

// replace makes the terminal send reply in place of each native reply that
// native matches.
func (h *terminalHarness) replace(native *regexp.Regexp, reply string) {
	h.mu.Lock()
	h.replacements = append(h.replacements, replyReplacement{native: native, reply: []byte(reply)})
	h.mu.Unlock()
}

// transcribe makes the terminal record everything it receives from its next
// read on.
func (h *terminalHarness) transcribe() {
	h.mu.Lock()
	h.transcript = new(bytes.Buffer)
	h.mu.Unlock()
}

// transcribed returns what the terminal has received since transcribe.
func (h *terminalHarness) transcribed() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.transcript.String()
}

func (h *terminalHarness) writeReplies(replies []byte) error {
	for len(replies) > 0 {
		select {
		case <-h.stop:
			return nil
		default:
		}
		n, err := unix.Write(h.fd, replies)
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			fds := []unix.PollFd{{Fd: int32(h.fd), Events: unix.POLLOUT}}
			if _, err := unix.Poll(fds, 20); err != nil && !errors.Is(err, unix.EINTR) {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		replies = replies[n:]
	}
	return nil
}

func (h *terminalHarness) fail(err error) { h.mu.Lock(); h.err = err; h.mu.Unlock() }

func (h *terminalHarness) text() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, err := h.em.State()
	if err != nil {
		return "state error: " + err.Error()
	}
	var b strings.Builder
	for _, c := range s.Cells {
		b.WriteString(c.Content)
	}
	return b.String()
}

func awaitText(t *testing.T, h *terminalHarness, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(h.text(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("outer display missing %q: %q", want, h.text())
}

func childCommand(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSessionChild$")
	cmd.Env = append(os.Environ(), "FRAME_TEST_CHILD=1", "TERM=xterm-256color")
	return cmd
}

func TestRunFramesRealChildAndPushesIdleUpdate(t *testing.T) {
	h := newHarness(t)
	before := termiosState(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	observed := make(chan struct{})
	snapshots := make(chan Snapshot, 1)
	app := New(childCommand(t), "Ready").Terminal(h.slave, h.slave).
		Header(1, func(c DrawContext[string]) {
			uv.NewStyledString(c.Data).Draw(c.View, c.View.Bounds())
			if c.Data == "Pushed while idle" {
				select {
				case snapshots <- c.Term:
				default:
				}
			}
		}).
		Footer(1, func(c DrawContext[string]) { uv.NewStyledString(c.Term.Terminal.Title).Draw(c.View, c.View.Bounds()) }).Border(true).
		Observe(func(e Event) {
			if e.Kind != Started {
				return
			}
			// Observer-owned containers must not corrupt a later drawing snapshot.
			s := e.Snapshot
			if s.Child.PTY.Window != nil {
				s.Child.PTY.Window.Col = 1
			}
			if len(s.Child.OperatingSystem.Args.Value) > 0 {
				s.Child.OperatingSystem.Args.Value[0] = "observer-owned"
			}
			if len(s.Terminal.Native.Cells) > 0 {
				s.Terminal.Native.Cells[0].Content = "observer-owned"
			}
			close(observed)
		})
	done := startRun(ctx, app)
	awaitText(t, h, "Ready")
	awaitText(t, h, "child")
	awaitText(t, h, "Child title")
	select {
	case <-observed:
	case <-ctx.Done():
		t.Fatal("Started was not observed", ctx.Err())
	}
	if err := app.InvalidateHeader("Pushed while idle"); err != nil {
		t.Fatal(err)
	}
	awaitText(t, h, "Pushed while idle")
	select {
	case s := <-snapshots:
		p := s.Child.OperatingSystem
		if p.PID != s.Child.PID || !p.SessionID.Available || p.SessionID.Value != s.Child.PID || !p.ProcessGroup.Available || p.ProcessGroup.Value != s.Child.PID {
			t.Fatalf("native child identity: %#v", p)
		}
		if !p.Args.Available || len(p.Args.Value) == 0 || p.Args.Value[0] != s.Child.Args[0] {
			t.Fatalf("native argv: %#v", p.Args)
		}
		found := false
		for _, member := range s.Child.SessionProcesses {
			found = found || member.PID == s.Child.PID
		}
		if !found || s.Child.SessionError != nil {
			t.Fatalf("owned session inventory: %#v, %v", s.Child.SessionProcesses, s.Child.SessionError)
		}
		pty := s.Child.PTY
		if pty.Window == nil || int(pty.Window.Col) != s.Viewport.Cols || int(pty.Window.Row) != s.Viewport.Rows || pty.Settings == nil || !pty.ForegroundGroup.Available || pty.ForegroundGroup.Value != s.Child.PID {
			t.Fatalf("native PTY metadata: %#v", pty)
		}
		if s.Terminal.Title != "Child title" || len(s.Terminal.Native.Cells) == 0 || s.Terminal.Native.Cells[0].Content == "observer-owned" {
			t.Fatalf("owned terminal snapshot: %#v", s.Terminal)
		}
	case <-ctx.Done():
		t.Fatal("pushed drawing snapshot was not delivered", ctx.Err())
	}
	if _, err := unix.Write(h.fd, []byte("q")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil || got.result.ProcessState == nil || !got.result.ProcessState.Success() || got.result.DrainError != nil || got.result.CleanupError != nil {
			t.Fatalf("Run = %#v, %v", got.result, got.err)
		}
	case <-ctx.Done():
		t.Fatal("Run did not finish", ctx.Err())
	}
	if after := termiosState(t, h); after != before {
		t.Fatalf("termios was not restored: before %s after %s", before, after)
	}
	if err := app.InvalidateHeader("closed"); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("closed invalidation: %v", err)
	}
}

func TestCancellationRestoresTerminalBeforeObserverDrains(t *testing.T) {
	h := newHarness(t)
	before := termiosState(t, h)
	ctx, cancel := context.WithCancelCause(context.Background())
	started, release := make(chan struct{}), make(chan struct{})
	app := New(childCommand(t), "Ready").Terminal(h.slave, h.slave).Header(1, func(c DrawContext[string]) { uv.NewStyledString(c.Data).Draw(c.View, c.View.Bounds()) }).Observe(func(e Event) {
		if e.Kind == Started {
			close(started)
			<-release
		}
	})
	done := make(chan error, 1)
	go func() { _, err := app.Run(ctx); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("child was not started")
	}
	cause := errors.New("test cancellation")
	cancel(cause)
	restored := awaitTermios(h, before, 3*time.Second)
	closed := errors.Is(app.InvalidateHeader("late"), ErrSessionClosed)
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, cause) {
			t.Errorf("cancellation cause: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("observer shutdown was not joined")
	}
	if !restored || !closed {
		t.Fatalf("terminal restored before observer drain=%v, controller closed=%v", restored, closed)
	}
}

func TestCancellationTerminatesAllOwnedSessionGroups(t *testing.T) {
	h := newHarness(t)
	cmd, file := descendantCommand(t, "groups")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan int, 1)
	done := make(chan error, 1)
	go func() {
		_, err := New(cmd, struct{}{}).Terminal(h.slave, h.slave).Observe(func(e Event) {
			if e.Kind == Started {
				started <- e.Snapshot.Child.PID
			}
		}).Run(ctx)
		done <- err
	}()
	var leader int
	select {
	case leader = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("leader did not start")
	}
	pid := awaitDescendantGroup(t, file, leader)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("cancel result: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled session did not complete")
	}
	awaitGone(t, pid, "owned descendant in separate process group survived cancellation")
}

// observerFault is a way for an observer callback to fail, with the error Run
// reports for it.
type observerFault struct {
	name string
	fail func()
	want error
}

// observerFaults lists the callback failures the dispatcher turns into session
// errors: a panic, reported with its value, and runtime.Goexit, which no
// recover stops.
func observerFaults(event EventKind) []observerFault {
	fault := fmt.Errorf("observer fault on %s", event)
	return []observerFault{
		{"panic", func() { panic(fault) }, fault},
		{"runtime.Goexit", runtime.Goexit, errObserverGoexit},
	}
}

func TestObserverFailureEndsSessionThroughShutdown(t *testing.T) {
	// The failing callback is a closure of this test, so its name appears in
	// the reported stack.
	test := t.Name()
	for _, fault := range observerFaults(Started) {
		t.Run(fault.name, func(t *testing.T) {
			h := newHarness(t)
			before := termiosState(t, h)
			cmd, file := descendantCommand(t, "groups")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			leaders, release := make(chan int, 1), make(chan struct{})
			// The child requests Kitty disambiguation, which the frame mirrors to
			// the outer terminal and only restoration disables there.
			app := New(cmd, struct{}{}).Terminal(h.slave, h.slave).
				ObserveEvents([]EventKind{Started}, func(e Event) {
					leaders <- e.Snapshot.Child.PID
					<-release
					fault.fail()
				})
			done := startRun(ctx, app)
			var leader int
			select {
			case leader = <-leaders:
			case <-ctx.Done():
				t.Fatal("Started was not observed", ctx.Err())
			}
			descendant := awaitDescendantGroup(t, file, leader)
			awaitOuter(t, h, "the framed session's modes", func(s emulator.State) bool {
				return s.Alternate && s.Modes[ghostty.ModeBracketedPaste] && s.KittyKeyboardFlags&ghostty.KittyKeyDisambiguate != 0
			})
			close(release)
			var got runOutcome
			select {
			case got = <-done:
			case <-ctx.Done():
				t.Fatal("Run did not finish after the observer failed", ctx.Err())
			}
			// The loop adopts the failure as its cancellation cause; Run's drain
			// join must not report it a second time.
			requireObserverFailure(t, got.err, fault.want, test)
			if got.result.ProcessState == nil {
				t.Fatal("Run did not wait for the terminated child")
			}
			awaitOuter(t, h, "its entry modes", func(s emulator.State) bool {
				return !s.Alternate && !s.Modes[ghostty.ModeBracketedPaste] && s.KittyKeyboardFlags == 0
			})
			if after := termiosState(t, h); after != before {
				t.Fatalf("termios was not restored: before %s after %s", before, after)
			}
			awaitGone(t, -leader, "launch leader's process group survived the observer failure")
			awaitGone(t, -descendant, "descendant process group survived the observer failure")
		})
	}
}

func TestObserverFailureDuringShutdownDrainReachesRun(t *testing.T) {
	// The failing callback is a closure of this test, so its name appears in
	// the reported stack.
	test := t.Name()
	for _, fault := range observerFaults(Exited) {
		t.Run(fault.name, func(t *testing.T) {
			h := newHarness(t)
			before := termiosState(t, h)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			exited, release := make(chan struct{}), make(chan struct{})
			app := New(childCommand(t), struct{}{}).Terminal(h.slave, h.slave).ObserveEvents([]EventKind{Exited}, func(Event) {
				close(exited)
				<-release
				fault.fail()
			})
			done := startRun(ctx, app)
			awaitText(t, h, "child")
			if _, err := unix.Write(h.fd, []byte("q")); err != nil {
				t.Fatal(err)
			}
			select {
			case <-exited:
			case <-ctx.Done():
				t.Fatal("Exited was not observed", ctx.Err())
			}
			// Restored termios proves the session loop has returned, so the failure
			// can reach Run only through the dispatcher's shutdown drain. Release
			// the observer before asserting so a failure cannot leave Run joined
			// forever.
			restored := awaitTermios(h, before, 5*time.Second)
			close(release)
			select {
			case got := <-done:
				if !restored {
					t.Fatal("terminal was not restored while the observer held Exited")
				}
				requireObserverFailure(t, got.err, fault.want, test)
				if got.result.ProcessState == nil || !got.result.ProcessState.Success() {
					t.Fatalf("child outcome = %v, want its successful exit", got.result.ProcessState)
				}
			case <-ctx.Done():
				t.Fatal("Run did not finish after the observer failed", ctx.Err())
			}
		})
	}
}

// A session error leaves child output unread. On Darwin the killed launch
// leader cannot finish exiting until that output drains, and while a process in
// another session holds the slave the drain has no deadline. Cleanup must not
// depend on either.
func TestSessionErrorCleanupIgnoresDetachedSlaveHolder(t *testing.T) {
	// Holding the first child output until cleanup has finished overflows the
	// observation queue, so the loop returns while the child is still writing.
	d := startDetached(t, "detached", nil, []EventKind{ChildOutput}, func(_ Event, hold <-chan struct{}) { <-hold })
	// Reading the unread output releases the leader at once. Finishing within
	// half the drain deadline rules out a release by the deadline's hangup.
	d.finish(t, awaitTermios(d.h, d.before, drainTimeout/2), ErrObservationOverflow)
}

// An observer still behind when cleanup reaps the launch leader overflows the
// queue again with Exited. That overflow is an observation failure like the
// one that ended the session, not a cleanup outcome: Run reports the overflow
// once, Result.CleanupError stays nil, and the observer never receives Exited.
func TestExitedOverflowDuringCleanupIsReportedOnce(t *testing.T) {
	var observed eventLog
	entered := make(chan struct{})
	var first sync.Once
	// The observer holds its callback for the child's first output until
	// cleanup has finished. Once the observer is inside that callback, the
	// test types the byte that starts the flood, which fills the queue behind
	// that callback and overflows it while the child still writes, so cleanup
	// reaps the leader and its Exited meets the same full queue.
	d := startDetached(t, "detached-on-input", nil, []EventKind{ChildOutput, Exited}, func(e Event, hold <-chan struct{}) {
		observed.record(e)
		first.Do(func() { close(entered) })
		<-hold
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the child's first output was not observed")
	}
	if _, err := unix.Write(d.h.fd, []byte("f")); err != nil {
		t.Fatal(err)
	}
	got := d.finish(t, awaitTermios(d.h, d.before, drainTimeout/2), ErrObservationOverflow)
	if n := strings.Count(got.err.Error(), ErrObservationOverflow.Error()); n != 1 {
		t.Fatalf("Run reported the overflow %d times, want once: %v", n, got.err)
	}
	if kinds := eventKinds(observed.events()); slices.Contains(kinds, Exited) {
		t.Fatalf("observer received %v, want no Exited after the overflow", kinds)
	}
}

// Output stopped by flow control cannot be read, so reading the child PTY
// cannot release a killed leader whose exit waits for that output; the drain
// deadline must.
func TestSessionErrorCleanupReleasesStoppedChildOutput(t *testing.T) {
	stopped := make(chan struct{})
	// The frame consumes all input but ^S; its observations, held until
	// cleanup has finished, overflow the queue.
	holding := false // Startup probe replies are outer input too; let them pass.
	d := startDetached(t, "detached", passOnlyStop, []EventKind{ChildInput, OuterInput}, func(e Event, hold <-chan struct{}) {
		switch {
		case e.Kind == ChildInput && bytes.Equal(e.Bytes, stopInput):
			holding = true
			close(stopped)
		case e.Kind == OuterInput && holding:
			<-hold
		}
	})
	d.stopOutput(t, stopped)
	// Separate reads each emit an observation; keep typing until the queue has
	// overflowed and cleanup has restored the terminal.
	cleaned := false
	for deadline := time.Now().Add(drainTimeout + 3*time.Second); !cleaned && time.Now().Before(deadline); {
		if _, err := unix.Write(d.h.fd, []byte("a")); err != nil && !errors.Is(err, unix.EAGAIN) {
			t.Fatal(err)
		}
		cleaned = awaitTermios(d.h, d.before, 5*time.Millisecond)
	}
	d.finish(t, cleaned, ErrObservationOverflow)
}

// Cancellation leaves the loop polling for the killed launch leader, whose exit
// on Darwin waits for output that flow control has stopped while a process in
// another session holds the slave. The wait for its reap must be bounded, and
// cleanup must share that bound rather than start its own.
func TestCancellationReleasesStoppedChildOutput(t *testing.T) {
	stopped := make(chan struct{})
	var observed eventLog
	d := startDetached(t, "detached", passOnlyStop, []EventKind{ChildInput, Exited}, func(e Event, _ <-chan struct{}) {
		observed.record(e)
		if e.Kind == ChildInput && bytes.Equal(e.Bytes, stopInput) {
			close(stopped)
		}
	})
	d.stopOutput(t, stopped)
	canceled := time.Now()
	d.cancel()
	// SIGKILL follows SIGTERM after terminationTimeout and the reap bound
	// drainTimeout after that. Half a drain deadline of margin fails a second
	// drainTimeout in cleanup.
	got := d.finish(t, awaitTermios(d.h, d.before, terminationTimeout+drainTimeout+drainTimeout/2), context.Canceled)
	// SIGTERM ends the child at once. The reap bound must not replace its exit
	// status with the hangup of the child PTY's close.
	if status, ok := got.result.ProcessState.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != syscall.SIGTERM {
		t.Fatalf("launch leader exit = %v, want the frame's SIGTERM", got.result.ProcessState)
	}
	// Cleanup reaps the leader only after closing the child PTY, which released
	// it. Exited must still report that exit, with a PTY sample cleanup took
	// before the close rather than one read through the closed descriptor. The
	// loop's last sample predates the reap deadline, which the loop returns at.
	pty := requireExited(t, observed.events(), got.result).Snapshot.Child.PTY
	reapDeadline := canceled.Add(terminationTimeout + drainTimeout)
	if pty.Window == nil || pty.WindowError != nil || pty.SettingsError != nil || pty.ObservedAt.Before(reapDeadline) {
		t.Fatalf("Exited PTY sample = %#v, want one taken at %v or later while the child PTY was open", pty, reapDeadline)
	}
}

// The session loop reaps a child that exits on its own. Cleanup reaps only a
// launch leader the loop left, so it must not report that exit again.
func TestChildExitReportsExitedOnce(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var observed eventLog
	app := New(childCommand(t), struct{}{}).Terminal(h.slave, h.slave).ObserveEvents([]EventKind{Started, Exited}, observed.record)
	done := startRun(ctx, app)
	awaitText(t, h, "child")
	if _, err := unix.Write(h.fd, []byte("q")); err != nil {
		t.Fatal(err)
	}
	var got runOutcome
	select {
	case got = <-done:
	case <-ctx.Done():
		t.Fatal("Run did not finish", ctx.Err())
	}
	if got.err != nil || got.result.ProcessState == nil || !got.result.ProcessState.Success() {
		t.Fatalf("Run = %#v, %v; want the child's successful exit", got.result, got.err)
	}
	events := observed.events()
	if kinds := eventKinds(events); !slices.Equal(kinds, []EventKind{Started, Exited}) {
		t.Fatalf("observer received %v, want Started then Exited", kinds)
	}
	requireExited(t, events, got.result)
}

// A session error that ends the session while the child runs leaves the launch
// leader to cleanup, which kills and reaps it. Exited must still follow Started
// once before Run returns, reporting the exit Result.ProcessState reports. A
// failed observer receives nothing after its failure, Exited included.
func TestSessionErrorReportsExitedBeforeRunReturns(t *testing.T) {
	type sessionError struct {
		name string
		// startup types the key before the terminal answers the frame's first
		// query, so the frame routes it after Started, before the session loop.
		startup bool
		// fault, when not nil, fails the observer on Started.
		fault *observerFault
	}
	cases := []sessionError{{name: "startup input", startup: true}, {name: "session loop input"}}
	for _, fault := range observerFaults(Started) {
		cases = append(cases, sessionError{name: "observer " + fault.name, startup: true, fault: &fault})
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			if tt.startup {
				h.precede(borderKey)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			started := make(chan struct{})
			var observed eventLog
			app := New(childCommand(t), struct{}{}).Terminal(h.slave, h.slave)
			// The header leaves the child one of the terminal's 12 rows, too few
			// for a border, so the border the key requests fails the session.
			app.Header(11, func(DrawContext[struct{}]) {}).Capture(func(in Input) Disposition {
				if bytes.Equal(in.Raw, borderKey) {
					if err := app.SetBorder(true); err != nil {
						t.Error(err)
					}
				}
				return Consume
			}).ObserveEvents([]EventKind{Started, Exited}, func(e Event) {
				observed.record(e)
				if e.Kind == Started {
					close(started)
					if tt.fault != nil {
						tt.fault.fail()
					}
				}
			})
			done := startRun(ctx, app)
			if !tt.startup {
				select {
				case <-started:
				case got := <-done:
					t.Fatalf("Run returned before the child started: %#v, %v", got.result, got.err)
				case <-ctx.Done():
					t.Fatal("Started was not observed", ctx.Err())
				}
				if _, err := unix.Write(h.fd, borderKey); err != nil {
					t.Fatal(err)
				}
			}
			var got runOutcome
			select {
			case got = <-done:
			case <-ctx.Done():
				t.Fatal("Run did not return after the session error", ctx.Err())
			}
			if !errors.Is(got.err, ErrViewportTooSmall) || got.result.CleanupError != nil {
				t.Fatalf("Run = %#v, %v; want %v and no cleanup error", got.result, got.err, ErrViewportTooSmall)
			}
			// Only cleanup's SIGKILL ends this child, so cleanup reaped it.
			if status, ok := got.result.ProcessState.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatalf("launch leader exit = %v, want cleanup's SIGKILL", got.result.ProcessState)
			}
			events := observed.events()
			if tt.fault != nil {
				if !errors.Is(got.err, tt.fault.want) || len(events) != 1 || events[0].Kind != Started {
					t.Fatalf("failed observer received %v; Run error %v, want only Started and %v", eventKinds(events), got.err, tt.fault.want)
				}
				return
			}
			if kinds := eventKinds(events); !slices.Equal(kinds, []EventKind{Started, Exited}) {
				t.Fatalf("observer received %v, want Started then Exited", kinds)
			}
			requireExited(t, events, got.result)
		})
	}
}

// borderKey is the key TestSessionErrorReportsExitedBeforeRunReturns captures
// to request a border.
var borderKey = []byte("b")

// eventLog records the events an observer receives.
type eventLog struct {
	mu       sync.Mutex
	recorded []Event
}

func (l *eventLog) record(e Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recorded = append(l.recorded, e)
}

// events returns the events recorded so far. Once Run has returned, they are
// every event the observer received.
func (l *eventLog) events() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.recorded)
}

// eventKinds returns the kind of each event, in order.
func eventKinds(events []Event) []EventKind {
	kinds := make([]EventKind, len(events))
	for i, e := range events {
		kinds[i] = e.Kind
	}
	return kinds
}

// requireExited checks that events hold exactly one Exited, reporting the
// launch leader's exit as result does, and returns it.
func requireExited(t *testing.T, events []Event, result Result) Event {
	t.Helper()
	var exited []Event
	for _, e := range events {
		if e.Kind == Exited {
			exited = append(exited, e)
		}
	}
	if len(exited) != 1 {
		t.Fatalf("observer received %d Exited events, want 1", len(exited))
	}
	state, want := exited[0].Snapshot.Child.ProcessState, result.ProcessState
	if state == nil || want == nil || state.String() != want.String() {
		t.Fatalf("Exited process state = %v, want Result.ProcessState %v", state, want)
	}
	return exited[0]
}

// stopInput is VSTOP (^S), which the default line discipline applies with IXON.
var stopInput = []byte{0x13}

// passOnlyStop passes only ^S to the child. Any other input would restart the
// child's output under IXANY, so the frame consumes it.
func passOnlyStop(in Input) Disposition {
	if bytes.Equal(in.Raw, stopInput) {
		return Pass
	}
	return Consume
}

// detachedRun is a session whose detached child writes output continuously
// while a sleeper in another session holds the child PTY's slave. A
// "detached" child floods from the start; a "detached-on-input" child first
// writes "ready" and floods once it reads a byte.
type detachedRun struct {
	h       *terminalHarness
	before  string
	holder  int
	done    <-chan runOutcome
	cancel  context.CancelFunc
	release func()
}

// startDetached starts a detachedRun whose child runs in mode, with capture,
// when not nil, and passes each observed event of kinds to observe. observe
// may block on hold, which closes once the test has seen whether cleanup
// finished.
func startDetached(t *testing.T, mode string, capture func(Input) Disposition, kinds []EventKind, observe func(e Event, hold <-chan struct{})) *detachedRun {
	t.Helper()
	h := newHarness(t)
	before := termiosState(t, h)
	cmd, file := descendantCommand(t, mode)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	leaders, hold := make(chan int, 1), make(chan struct{})
	release := sync.OnceFunc(func() { close(hold) })
	t.Cleanup(release)
	app := New(cmd, struct{}{}).Terminal(h.slave, h.slave)
	if capture != nil {
		app.Capture(capture)
	}
	app.ObserveEvents(append([]EventKind{Started}, kinds...), func(e Event) {
		if e.Kind == Started {
			leaders <- e.Snapshot.Child.PID
			return
		}
		observe(e, hold)
	})
	done := startRun(ctx, app)
	var leader int
	select {
	case leader = <-leaders:
	case got := <-done:
		t.Fatalf("Run returned before the child started: %#v, %v", got.result, got.err)
	case <-ctx.Done():
		t.Fatal("Started was not observed", ctx.Err())
	}
	holder := awaitDescendantGroup(t, file, leader)
	if sid, err := unix.Getsid(holder); err != nil || sid != holder {
		t.Fatalf("holder session = %d, %v; want its own session %d", sid, err, holder)
	}
	return &detachedRun{h: h, before: before, holder: holder, done: done, cancel: cancel, release: release}
}

// stopOutput types ^S on the outer terminal and waits until stopped reports
// that the frame wrote it to the child, which stops the child's output. The
// session's capture must be passOnlyStop.
func (d *detachedRun) stopOutput(t *testing.T, stopped <-chan struct{}) {
	t.Helper()
	if _, err := unix.Write(d.h.fd, stopInput); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("^S did not reach the child")
	}
}

// finish requires that child cleanup restored the terminal, which Run does
// only after cleanup has returned, and that Run then reported want with the
// leader's exit state while the holder survived. It returns Run's outcome.
func (d *detachedRun) finish(t *testing.T, cleaned bool, want error) runOutcome {
	t.Helper()
	if !cleaned {
		// Ending the holder bounds the leader's drain, so Run can return before
		// the test reports the failure.
		if err := unix.Kill(-d.holder, syscall.SIGKILL); err != nil {
			t.Error(err)
		}
	}
	d.release()
	var got runOutcome
	select {
	case got = <-d.done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its observer was released")
	}
	if !cleaned {
		t.Fatal("child cleanup did not finish in time while a process in another session held the child PTY")
	}
	if !errors.Is(got.err, want) || got.result.ProcessState == nil || got.result.CleanupError != nil {
		t.Fatalf("Run = %#v, %v; want %v, the launch leader's exit state and no cleanup error", got.result, got.err, want)
	}
	if err := unix.Kill(d.holder, 0); err != nil {
		t.Fatalf("detached holder did not outlive Run: %v", err)
	}
	return got
}

// descendantCommand returns a child in mode, "groups", "detached" or
// "detached-on-input", whose descendant leads its own process group and writes
// its PID to the returned file. Cleanup kills that group once its PID is
// published, even when the test failed before reading it.
func descendantCommand(t *testing.T, mode string) (*exec.Cmd, string) {
	t.Helper()
	dir, err := os.MkdirTemp(".tmp", "session-"+mode+"-")
	if err != nil {
		t.Fatal(err)
	}
	file := dir + "/pid"
	t.Cleanup(func() {
		if data, err := os.ReadFile(file); err == nil {
			pid, err := strconv.Atoi(string(data))
			if err != nil {
				t.Error(err)
			} else if err := unix.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
				t.Error(err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Error(err)
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	cmd := childCommand(t)
	cmd.Env = append(cmd.Env, "FRAME_TEST_MODE="+mode, "FRAME_TEST_PID_FILE="+file)
	return cmd, file
}

// awaitDescendantGroup reads the PID descendantCommand's descendant records and
// verifies that it leads its own process group, apart from the launch leader.
func awaitDescendantGroup(t *testing.T, file string, leader int) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	pid := 0
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(file)
		if err == nil {
			pid, err = strconv.Atoi(string(data))
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("separate child group did not start")
	}
	if pgid, err := unix.Getpgid(pid); err != nil || pgid != pid || pgid == leader {
		t.Fatalf("group = %d, %v; leader=%d descendant=%d", pgid, err, leader, pid)
	}
	return pid
}

// awaitGone waits until kill(2) finds no process for target, a PID or a
// negated process-group ID.
func awaitGone(t *testing.T, target int, failure string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := unix.Kill(target, 0); errors.Is(err, unix.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(failure)
}

// awaitOuter waits until the outer terminal's native state satisfies ready.
func awaitOuter(t *testing.T, h *terminalHarness, what string, ready func(emulator.State) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		s, err := h.em.State()
		h.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if ready(s) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("outer terminal did not reach %s", what)
}

// runOutcome holds one Run call's return values.
type runOutcome struct {
	result Result
	err    error
}

// startRun executes app's session on its own goroutine and delivers Run's
// return values when it returns.
func startRun[T any](ctx context.Context, app *Frame[T]) <-chan runOutcome {
	done := make(chan runOutcome, 1)
	go func() {
		r, err := app.Run(ctx)
		done <- runOutcome{r, err}
	}()
	return done
}

// termiosState returns the outer terminal's line discipline in a form that is
// equal only for identical settings.
func termiosState(t *testing.T, h *terminalHarness) string {
	t.Helper()
	s, err := term.GetState(h.slave.Fd())
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%#v", s)
}

// awaitTermios reports whether the outer terminal's line discipline returns to
// want within timeout. It does not fail the test, so a caller can first
// release a callback that holds the session open.
func awaitTermios(h *terminalHarness, want string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s, err := term.GetState(h.slave.Fd())
		if err == nil && fmt.Sprintf("%#v", s) == want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// requireObserverFailure checks that Run reported fault exactly once, with the
// stack of the failing callback, which the test function named test encloses.
func requireObserverFailure(t *testing.T, err, fault error, test string) {
	t.Helper()
	if !errors.Is(err, fault) || !strings.Contains(err.Error(), test) {
		t.Fatalf("Run error = %v, want %q and the callback's stack", err, fault)
	}
	// Each report carries the fault's text once; the stack prints function
	// names and argument words, not that text.
	if n := strings.Count(err.Error(), fault.Error()); n != 1 {
		t.Fatalf("Run reported the observer failure %d times, want once: %v", n, err)
	}
}
