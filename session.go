package frame

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alexgorbatchev/go-tui-frame/internal/emulator"
	"github.com/alexgorbatchev/go-tui-frame/internal/input"
	"github.com/alexgorbatchev/go-tui-frame/internal/process"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"
	ghostty "go.mitchellh.com/libghostty"
	"golang.org/x/sys/unix"
)

const (
	inputQueueLimit    = 1 << 20
	readBufferSize     = 64 << 10
	escapeTimeout      = 50 * time.Millisecond
	renderHoldTimeout  = 250 * time.Millisecond
	terminationTimeout = time.Second
	drainTimeout       = 2 * time.Second
	maxScreenCells     = 1 << 20
	// endpointTerminfo names the terminfo entry the virtual endpoint
	// implements: the child's TERM and the endpoint's XTGETTCAP TN reply.
	endpointTerminfo = "xterm-256color"
)

// Run executes the child in a native PTY and owns one outer-terminal session.
// Child exit status is returned in Result; errors describe session operations.
func (f *Frame[T]) Run(ctx context.Context) (result Result, err error) {
	if err = f.begin(ctx); err != nil {
		return result, err
	}
	defer f.close()
	// An observer panic or runtime.Goexit cancels the session as the caller's
	// context does, so it ends through the same child termination and terminal
	// restoration.
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	events := newEventDispatcher(f.observedKinds, cancel, f.observe)
	defer func() {
		events.close()
		// A callback can fail while close drains admitted observations, after
		// the session loop stopped reading its context.
		err = joinOnce(err, events.failure())
	}()
	// Go ignores SIGWINCH while no channel is registered, so the subscription
	// precedes reading the outer size: a resize during startup stays buffered
	// until the loop's monitor turns it into a resize.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	fd, device, w, err := inspectConsole(f.input, f.output)
	if err != nil {
		return result, err
	}
	g, err := f.checkedLayout(w)
	if err != nil {
		return result, err
	}
	c, err := acquireConsole(f.input, f.output, fd, device)
	if err != nil {
		return result, err
	}
	c.inherit = f.inheritTerminal
	defer func() {
		result.CleanupError = errors.Join(result.CleanupError, c.restore())
		f.close()
		err = errors.Join(err, result.CleanupError)
	}()
	saved, err := c.probe(ctx, events, f.probeTimeout)
	if err != nil {
		return result, err
	}
	if c.cellWidth == 0 && w.Col > 0 {
		c.cellWidth = uint32(w.Xpixel) / uint32(w.Col)
	}
	if c.cellHeight == 0 && w.Row > 0 {
		c.cellHeight = uint32(w.Ypixel) / uint32(w.Row)
	}
	size := c.childSize(g)
	em, err := emulator.New(c.childOptions(size))
	if err != nil {
		return result, err
	}
	defer em.Close()
	router, err := newInputRouter(em, f.capture)
	if err != nil {
		return result, err
	}
	defer router.close()
	s := &session[T]{frame: f, console: c, terminal: em, router: router, events: events, geometry: g, screen: c.outerScreen(g), framer: c.framer, winch: winch}
	if err = s.openWake(); err != nil {
		return result, err
	}
	defer func() { result.CleanupError = errors.Join(result.CleanupError, s.closeWake()) }()
	defer func() { result.CleanupError = errors.Join(result.CleanupError, s.hangup.close()) }()
	f.cmd.Env = childEnvironment(f.cmd.Environ())
	if ctx.Err() != nil {
		return result, context.Cause(ctx)
	}
	s.master, err = c.startChild(f.cmd, c.childWinsize(g))
	if err != nil {
		return result, fmt.Errorf("start child PTY: %w", err)
	}
	s.fd = int(s.master.Fd())
	s.wait = make(chan error, 1)
	s.waitDone = make(chan struct{})
	go func() { defer close(s.waitDone); s.wait <- f.cmd.Wait(); s.wakeLoop() }()
	defer func() {
		cleanupErr, exitedErr := s.cleanupChild()
		result.CleanupError = errors.Join(result.CleanupError, cleanupErr)
		result.ProcessState = f.cmd.ProcessState
		result.DrainError = s.drainErr
		// An observer that overflowed the queue and ended the session is
		// usually still behind when cleanup emits Exited.
		err = joinOnce(err, exitedErr)
	}()
	defer f.close()
	if err = unix.SetNonblock(s.fd, true); err != nil {
		return result, fmt.Errorf("set child PTY nonblocking: %w", err)
	}
	s.snapshot = Snapshot{Child: ChildSnapshot{PID: f.cmd.Process.Pid, Executable: f.cmd.Path, Args: slices.Clone(f.cmd.Args), Environment: slices.Clone(f.cmd.Env), Directory: f.cmd.Dir, StartedAt: time.Now()}, Outer: Size{Cols: g.outer.Dx(), Rows: g.outer.Dy()}, Viewport: Size{Cols: g.child.Dx(), Rows: g.child.Dy()}}
	if err = c.enter(); err != nil {
		return result, fmt.Errorf("enter terminal session: %w", err)
	}
	if err = s.refresh(true); err != nil {
		return result, err
	}
	if err = events.emit(Event{Kind: Started, Snapshot: &s.snapshot}); err != nil {
		return result, err
	}
	s.started = true
	if err = s.hold(saved...); err != nil {
		return result, err
	}
	if s.framer.NeedsDeadline() {
		s.escapeDeadline = time.Now().Add(escapeTimeout)
	}
	return result, s.loop(ctx)
}

// joinOnce joins err to joined unless joined already holds it, so a failure
// that several steps meet, such as a full observation queue, is reported once.
func joinOnce(joined, err error) error {
	if err == nil || errors.Is(joined, err) {
		return joined
	}
	return errors.Join(joined, err)
}

type pendingInput struct {
	bytes  []byte
	origin string
	offset int
}

type session[T any] struct {
	frame                                                     *Frame[T]
	console                                                   *console
	terminal                                                  *emulator.Terminal
	router                                                    *inputRouter
	events                                                    *eventDispatcher
	geometry                                                  geometry
	screen                                                    uv.ScreenBuffer
	snapshot                                                  Snapshot
	state                                                     emulator.State
	nextState, inputState                                     emulator.State
	statePending, composed                                    bool
	paintedCursor                                             Cursor
	paintedNativeCursor                                       ghostty.RenderStateCursor
	paintedCursorColor                                        ghostty.ColorRGB
	paintedCursorColorSet                                     bool
	metadataDirty, scrollDirty                                bool
	master                                                    *os.File
	fd                                                        int
	wakeRead, wakeWrite                                       *os.File
	wakeReadFD, wakeWriteFD                                   int
	wakeMu                                                    sync.Mutex
	wakeErr                                                   error
	wait                                                      chan error
	waitDone                                                  chan struct{}
	waited, ptyEOF                                            bool
	started, terminationStarted                               bool
	drainErr                                                  error
	queue                                                     []pendingInput
	held                                                      []input.Packet
	hangup                                                    hangupWatch
	inputBuffers                                              [][]byte
	inputBufferBytes                                          int
	queuedBytes                                               int
	framer                                                    *input.Framer
	escapeDeadline, holdDeadline, killDeadline, drainDeadline time.Time
	renderDeadline, reapDeadline                              time.Time
	// winch receives the outer terminal's SIGWINCH from before Run reads the
	// outer size until Run returns.
	winch <-chan os.Signal
}

// endpointConflictVariables names outer-terminal variables the child must not
// inherit. The child talks to the virtual endpoint, and each identity here is
// read by at least one known program to pick protocols or escape sequences the
// endpoint does not provide, or to size itself to the outer terminal. The list
// is not exhaustive: other inherited variables can still steer programs. A
// variable that only names the outer terminal passes through, and so does
// VSCODE_GIT_ASKPASS_MAIN, which grok-build reads to trust OSC 52 writes but
// which VS Code's git askpass helper runs. Names match exactly: multiplexer
// and remote-control handles such as TMUX, TMUX_PANE, WEZTERM_UNIX_SOCKET,
// KITTY_LISTEN_ON, and ALACRITTY_SOCKET pass through, so tools in the child
// can still address the outer multiplexer or terminal. KITTY_WINDOW_ID,
// WEZTERM_PANE, and ITERM_SESSION_ID also act as default targets for remote
// control, but image tools read them to emit graphics into the child's
// display, so they are removed and remote-control commands in the child need
// an explicit target.
var endpointConflictVariables = map[string]bool{
	// The endpoint's own TERM and COLORTERM replace the outer ones.
	"TERM":      true,
	"COLORTERM": true,
	// Terminal identity that image tools such as chafa, yazi, viuer, pi,
	// odiff, and go-termimg map to Kitty graphics, Sixel, or iTerm2 inline
	// images. The endpoint disables Kitty graphics and implements neither
	// Sixel nor iTerm2's OSC 1337 File.
	"TERM_PROGRAM":                true,
	"LC_TERMINAL":                 true,
	"KONSOLE_VERSION":             true,
	"KITTY_WINDOW_ID":             true,
	"KITTY_PID":                   true,
	"GHOSTTY_RESOURCES_DIR":       true,
	"GHOSTTY_BIN_DIR":             true,
	"WEZTERM_EXECUTABLE":          true,
	"WEZTERM_PANE":                true,
	"ITERM_SESSION_ID":            true,
	"TERM_SESSION_ID":             true,
	"XTERM_VERSION":               true,
	"MLTERM":                      true,
	"TERMINAL_NAME":               true,
	"EAT_SHELL_INTEGRATION_DIR":   true,
	"WARP_HONOR_PS1":              true,
	"WARP_SESSION_ID":             true,
	"WARP_TERMINAL_SESSION_UUID":  true,
	"WARP_IS_LOCAL_SHELL_SESSION": true,
	"VSCODE_INJECTION":            true,
	"TABBY_CONFIG_DIRECTORY":      true,
	// Terminal-specific integrations: VTE's vte.sh emits VTE's OSC 666
	// properties, and Ghostty's shell integration makes ssh request its
	// xterm-ghostty terminfo on remote hosts.
	"VTE_VERSION":            true,
	"GHOSTTY_SHELL_FEATURES": true,
	// Terminal identity that makes programs such as grok-build trust OSC 52
	// clipboard writes, which the endpoint refuses.
	"WT_SESSION":      true,
	"ITERM_PROFILE":   true,
	"CURSOR_TRACE_ID": true,
	// Companions: the version or feature report of a removed identity
	// describes the same outer terminal, so it goes with that identity.
	// TERM_PROGRAM_VERSION versions TERM_PROGRAM, LC_TERMINAL_VERSION versions
	// LC_TERMINAL, and TERM_FEATURES is the feature report of iTerm2, the
	// terminal TERM_PROGRAM, LC_TERMINAL, and ITERM_SESSION_ID identify.
	"TERM_PROGRAM_VERSION": true,
	"LC_TERMINAL_VERSION":  true,
	"TERM_FEATURES":        true,
	// The outer size. ncurses prefers exported COLUMNS and LINES to the PTY
	// window size and then ignores SIGWINCH, so the child would size itself to
	// the outer terminal for the whole session.
	"COLUMNS": true,
	"LINES":   true,
}

// childEnvironment returns env without the variables that conflict with the
// virtual endpoint and with the endpoint's TERM and COLORTERM.
func childEnvironment(env []string) []string {
	out := make([]string, 0, len(env)+2)
	for _, v := range env {
		name, _, _ := strings.Cut(v, "=")
		if !endpointConflictVariables[name] {
			out = append(out, v)
		}
	}
	return append(out, "TERM="+endpointTerminfo, "COLORTERM=truecolor")
}

func (f *Frame[T]) checkedLayout(w *unix.Winsize) (geometry, error) {
	if int(w.Col)*int(w.Row) > maxScreenCells {
		return geometry{}, fmt.Errorf("terminal exceeds %d-cell screen budget", maxScreenCells)
	}
	return f.layout(Size{Cols: int(w.Col), Rows: int(w.Row)})
}

func (c *console) childSize(g geometry) emulator.Size {
	return emulator.Size{Cols: g.child.Dx(), Rows: g.child.Dy(), CellWidthPx: c.cellWidth, CellHeightPx: c.cellHeight}
}

func (c *console) childWinsize(g geometry) *pty.Winsize {
	return &pty.Winsize{Cols: uint16(g.child.Dx()), Rows: uint16(g.child.Dy()), X: uint16(min(uint32(g.child.Dx())*c.cellWidth, 65535)), Y: uint16(min(uint32(g.child.Dy())*c.cellHeight, 65535))}
}

// outerScreen allocates the outer composition buffer, measuring text the way
// the outer terminal does.
func (c *console) outerScreen(g geometry) uv.ScreenBuffer {
	screen := uv.NewScreenBuffer(g.outer.Dx(), g.outer.Dy())
	screen.Method = ansi.WcWidth
	if c.graphemeWidth() {
		screen.Method = ansi.GraphemeWidth
	}
	return screen
}

func (s *session[T]) refresh(force bool) error {
	if err := s.refreshState(); err != nil {
		return err
	}
	return s.render(force)
}

// refreshState captures the virtual terminal's state and synchronizes outer
// input modes and routing with it.
func (s *session[T]) refreshState() error {
	if err := s.captureState(); err != nil {
		return err
	}
	state := s.state
	// Keep separate routing maps: per-read updates must not mutate the last
	// metadata sample used to decide whether region callbacks need redrawing.
	input := &s.inputState
	if input.Modes == nil {
		input.Modes = make(map[ghostty.Mode]bool, len(state.Modes))
		input.ModeErrors = make(map[ghostty.Mode]error)
	}
	clear(input.Modes)
	clear(input.ModeErrors)
	maps.Copy(input.Modes, state.Modes)
	maps.Copy(input.ModeErrors, state.ModeErrors)
	input.Size, input.Held, input.Alternate = state.Size, state.Held, state.Alternate
	input.KittyKeyboardFlags, input.ModifyOtherKeys2 = state.KittyKeyboardFlags, state.ModifyOtherKeys2
	input.MouseTracking, input.MouseTrackingMode = state.MouseTracking, state.MouseTrackingMode
	return s.syncInput()
}

// captureState copies the virtual terminal's state into the snapshot.
func (s *session[T]) captureState() error {
	if err := s.terminal.UpdateState(&s.nextState); err != nil {
		return err
	}
	state := s.nextState
	s.metadataDirty = s.metadataDirty || regionMetadataChanged(s.state, state)
	s.scrollDirty = s.scrollDirty || scrollbackChanged(s.state, state)
	s.nextState, s.state = s.state, state
	s.statePending = false
	s.snapshot.ObservedAt = time.Now()
	s.snapshot.Terminal = TerminalSnapshot{Title: state.Title, Directory: state.Directory, Size: Size{Cols: state.Size.Cols, Rows: state.Size.Rows}, Cells: state.Cells, Cursor: Cursor{X: int(state.Cursor.ViewportX), Y: int(state.Cursor.ViewportY), Visible: state.Cursor.Visible && state.Cursor.ViewportHasValue}, Alternate: state.Alternate, Native: state}
	return nil
}

func (s *session[T]) refreshInput() error {
	if err := s.terminal.InputState(&s.inputState); err != nil {
		return err
	}
	if err := s.syncInput(); err != nil {
		return err
	}
	// Host protocol changes must reach the terminal before the next input
	// packet is decoded using the updated profile. Viewport output stays queued.
	if err := s.console.renderer.Flush(); err != nil {
		return fmt.Errorf("synchronize outer input modes: %w", err)
	}
	return nil
}

func (s *session[T]) syncInput() error {
	if err := s.console.syncInput(s.inputState); err != nil {
		return err
	}
	if s.inputState.Held && s.holdDeadline.IsZero() {
		s.holdDeadline = time.Now().Add(renderHoldTimeout)
	}
	if !s.inputState.Held {
		s.holdDeadline = time.Time{}
	}
	return nil
}

func (s *session[T]) render(force bool) error {
	if s.statePending {
		if err := s.refreshState(); err != nil {
			return err
		}
	}
	if s.sampleDue(force) {
		s.collectMetadata()
	}
	s.frame.compose(&s.screen, s.geometry, s.snapshot, repaintDamage{
		full: !s.composed, regions: force || s.metadataDirty || s.scrollDirty, rows: s.terminal.DirtyRows(),
	})
	c := s.console
	cursor := s.snapshot.Terminal.Cursor
	native := s.snapshot.Terminal.Native
	appearanceChanged := native.Cursor != s.paintedNativeCursor || native.Colors.Cursor != s.paintedCursorColor || native.Colors.CursorHasValue != s.paintedCursorColorSet
	cellsChanged := !s.composed || screenChanged(s.screen)
	if cellsChanged || cursor != s.paintedCursor || appearanceChanged {
		if err := c.setMode(synchronizedOutputMode, true); err != nil {
			return err
		}
		if err := c.syncCursor(s.snapshot.Terminal.Native); err != nil {
			return fmt.Errorf("render child cursor: %w", err)
		}
		if cellsChanged {
			c.renderer.Render(s.screen.RenderBuffer)
		}
		if cursor.Visible {
			c.renderer.MoveTo(s.geometry.child.Min.X+cursor.X, s.geometry.child.Min.Y+cursor.Y)
		}
		if err := c.setMode(25, cursor.Visible); err != nil {
			return err
		}
		if err := c.setMode(synchronizedOutputMode, false); err != nil {
			return err
		}
	}
	if err := c.renderer.Flush(); err != nil {
		return fmt.Errorf("render outer terminal: %w", err)
	}
	s.metadataDirty, s.scrollDirty = false, false
	s.composed = true
	s.paintedCursor = cursor
	s.paintedNativeCursor = native.Cursor
	s.paintedCursorColor, s.paintedCursorColorSet = native.Colors.Cursor, native.Colors.CursorHasValue
	s.terminal.ClearDamage()
	s.renderDeadline = time.Time{}
	return nil
}

// hold appends framed outer input behind any input already held and routes
// what the child input queue can take.
func (s *session[T]) hold(packets ...input.Packet) error {
	s.held = append(s.held, packets...)
	return s.routeHeld()
}

// routeHeld routes held outer input in order while the child input queue has
// room for each packet's worst-case routing. Input that does not fit stays
// held, and outer reads stay paused, until the child drains the queue.
func (s *session[T]) routeHeld() error {
	var err error
	n := 0
	for err == nil && n < len(s.held) && s.admits(s.held[n]) {
		err = s.route(s.held[n])
		n++
	}
	kept := copy(s.held, s.held[n:])
	clear(s.held[kept:])
	s.held = s.held[:kept]
	return err
}

// releaseHeld routes held input after the child drained part of its queue and
// reports whether it routed any. Outer reads resume once nothing is held. A
// pending Escape then gets a fresh deadline because bytes completing its
// sequence may have waited unread.
func (s *session[T]) releaseHeld() (bool, error) {
	held := len(s.held)
	if held == 0 {
		return false, nil
	}
	if err := s.routeHeld(); err != nil {
		return true, err
	}
	if !s.inputPaused() && !s.escapeDeadline.IsZero() {
		s.escapeDeadline = time.Now().Add(escapeTimeout)
	}
	return len(s.held) < held, nil
}

// inputPaused reports whether framed outer input waits for queue room, which
// stops further outer reads.
func (s *session[T]) inputPaused() bool {
	return len(s.held) > 0
}

// admits reports whether routing p cannot overflow the child input queue. A
// packet whose worst case exceeds the whole queue waits for an empty queue;
// enqueue still checks the bytes it actually routes.
func (s *session[T]) admits(p input.Packet) bool {
	return min(routedLimit(p), inputQueueLimit) <= inputQueueLimit-s.queuedBytes
}

func (s *session[T]) route(p input.Packet) error {
	if err := s.updateLayout(); err != nil {
		return err
	}
	width, height := s.console.cellWidth, s.console.cellHeight
	if s.console.consumeReply(p) {
		// A zero or repeated cell-size report leaves the measured cells, and so
		// the child's geometry, as they are.
		if s.console.cellWidth != width || s.console.cellHeight != height {
			return s.applyGeometry(s.geometry)
		}
		return s.console.syncInput(s.inputState)
	}
	s.router.viewport = s.geometry.child
	s.router.host = hostInputProfile{KittyFlags: s.console.kittyFlags, MousePixels: s.console.applied[1016], CellWidthPx: s.console.cellWidth, CellHeightPx: s.console.cellHeight}
	r, err := s.router.route(p, s.inputState)
	if errors.Is(err, errUnroutable) {
		// One event the child's protocol cannot carry is withheld, as a
		// terminal connected directly to the child could not send it either.
		return s.events.emit(Event{Kind: Routed, Bytes: p.Raw, Origin: "unroutable-input", Disposition: Pass, Error: err})
	}
	if err != nil {
		return err
	}
	// Capture can schedule geometry changes. Apply them before another packet
	// in the same read uses the child's coordinates or generated query state.
	if err := s.updateLayout(); err != nil {
		return err
	}
	if r.Disposition == Consume {
		if k, ok := p.Event.(uv.KeyEvent); ok {
			in := Input{Raw: p.Raw, Key: k}
			return s.events.emit(Event{Kind: Captured, Bytes: p.Raw, Input: &in, Disposition: Consume, Origin: r.Origin})
		}
	}
	if len(r.Bytes) == 0 || r.Origin != "user-input" {
		if err := s.events.emit(Event{Kind: Routed, Bytes: p.Raw, Origin: r.Origin, Disposition: r.Disposition}); err != nil {
			return err
		}
	}
	return s.enqueue(r.Bytes, r.Origin)
}

func (s *session[T]) enqueue(data []byte, origin string) error {
	if len(data) == 0 || s.waited {
		return nil
	}
	// Outer input is admitted only within its worst-case room, so for it this
	// guards an invariant. Terminal replies bypass admission: holding them back
	// would mean pausing child output, and a child blocked writing output may
	// never read its input.
	if len(data) > inputQueueLimit-s.queuedBytes {
		return fmt.Errorf("child input exceeded %d-byte queue", inputQueueLimit)
	}
	n := len(s.queue)
	if n > 0 && s.queue[n-1].origin == origin {
		p := &s.queue[n-1]
		if p.offset > 0 {
			copy(p.bytes, p.bytes[p.offset:])
			p.bytes = p.bytes[:len(p.bytes)-p.offset]
			p.offset = 0
		}
		p.bytes = append(p.bytes, data...)
	} else {
		var buf []byte
		if n := len(s.inputBuffers); n > 0 {
			buf = s.inputBuffers[n-1]
			s.inputBufferBytes -= cap(buf)
			s.inputBuffers[n-1] = nil
			s.inputBuffers = s.inputBuffers[:n-1]
		}
		s.queue = append(s.queue, pendingInput{bytes: append(buf[:0], data...), origin: origin})
	}
	s.queuedBytes += len(data)
	return nil
}

func (s *session[T]) writeInput() error {
	if len(s.queue) == 0 {
		return nil
	}
	p := &s.queue[0]
	n, err := unix.Write(s.fd, p.bytes[p.offset:])
	if n > 0 {
		if e := s.events.emit(Event{Kind: ChildInput, Bytes: p.bytes[p.offset : p.offset+n], Origin: p.origin}); e != nil {
			return e
		}
		p.offset += n
		s.queuedBytes -= n
	}
	if p.offset == len(p.bytes) {
		if cap(p.bytes) <= inputQueueLimit-s.inputBufferBytes {
			s.inputBuffers = append(s.inputBuffers, p.bytes[:0])
			s.inputBufferBytes += cap(p.bytes)
		}
		copy(s.queue, s.queue[1:])
		s.queue[len(s.queue)-1] = pendingInput{}
		s.queue = s.queue[:len(s.queue)-1]
	}
	if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("write child PTY: %w", err)
	}
	return nil
}

// kittyKeyboardReply matches the whole reply the child's native terminal
// writes for a Kitty keyboard query (CSI ? u): CSI ? flags u from its own
// Kitty stack. Each native reply write is one Reply effect.
var kittyKeyboardReply = regexp.MustCompile(`\A\x1b\[\?[0-9]+u\z`)

func (s *session[T]) effects() error {
	for _, e := range s.terminal.Effects() {
		if e.Kind == emulator.Reply && !s.console.kittySupported && kittyKeyboardReply.Match(e.Bytes) {
			// The child would enable a protocol the outer terminal never sends.
			// Unanswered, the query reads as a terminal without it, so the reply
			// is neither written nor observed. A Kitty reply from the outer
			// terminal after the probe deadline makes later queries answered.
			continue
		}
		if e.Kind == emulator.Reply {
			if err := s.enqueue(e.Bytes, "terminal-reply"); err != nil {
				return err
			}
		}
		if err := s.events.emit(Event{Kind: Protocol, Origin: string(e.Kind), Effect: &e}); err != nil {
			return err
		}
		if e.Kind == emulator.Bell {
			if _, err := s.console.renderer.WriteString("\a"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *session[T]) readChild(buf []byte) error {
	n, err := unix.Read(s.fd, buf)
	if n > 0 {
		if e := s.events.emit(Event{Kind: ChildOutput, Bytes: buf[:n], Origin: "pty"}); e != nil {
			return e
		}
		if _, e := s.terminal.Write(buf[:n]); e != nil {
			return e
		}
		if e := s.effects(); e != nil {
			return e
		}
		if e := s.refreshInput(); e != nil {
			return e
		}
		s.statePending = true
		// Keep the first deadline so a continuously writing child cannot postpone
		// display indefinitely. Protocol replies and observations remain immediate.
		if s.renderDeadline.IsZero() {
			s.renderDeadline = time.Now().Add(renderInterval)
		}
		if s.events.wants(StateChanged) {
			if e := s.refreshState(); e != nil {
				return e
			}
			if e := s.events.emit(Event{Kind: StateChanged, Snapshot: &s.snapshot}); e != nil {
				return e
			}
		}
	}
	return s.childReadError(n, err)
}

// childReadError records the end of child output on EOF or EIO and returns
// the error of a child PTY read that cannot be retried.
func (s *session[T]) childReadError(n int, err error) error {
	if n == 0 && err == nil || errors.Is(err, unix.EIO) {
		s.ptyEOF = true
		return nil
	}
	if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read child PTY: %w", err)
	}
	return nil
}

func (s *session[T]) readOuter(buf []byte) error {
	n, err := unix.Read(s.console.fd, buf)
	if err != nil {
		if errors.Is(err, unix.EINTR) {
			return nil
		}
		return fmt.Errorf("read outer input: %w", err)
	}
	if n == 0 {
		return errors.New("outer terminal input ended")
	}
	if err := s.events.emit(Event{Kind: OuterInput, Bytes: buf[:n], Origin: "terminal"}); err != nil {
		return err
	}
	packets, err := s.framer.Feed(buf[:n])
	if err != nil {
		return err
	}
	if err := s.hold(packets...); err != nil {
		return err
	}
	s.escapeDeadline = time.Time{}
	if s.framer.NeedsDeadline() {
		s.escapeDeadline = time.Now().Add(escapeTimeout)
	}
	return nil
}

func (s *session[T]) resize() error {
	w, err := unix.IoctlGetWinsize(s.console.fd, unix.TIOCGWINSZ)
	if err != nil {
		return err
	}
	g, err := s.frame.checkedLayout(w)
	if err != nil {
		return err
	}
	width, height := uint32(w.Xpixel)/uint32(w.Col), uint32(w.Ypixel)/uint32(w.Row)
	if g == s.geometry && width == s.console.cellWidth && height == s.console.cellHeight {
		return nil
	}
	s.console.cellWidth, s.console.cellHeight = width, height
	if width == 0 || height == 0 {
		if _, err := s.console.renderer.WriteString(ansi.WindowOp(16)); err != nil {
			return err
		}
	}
	return s.applyGeometry(g)
}

func (s *session[T]) updateLayout() error {
	if !s.frame.takeLayoutChange() {
		return nil
	}
	g, err := s.frame.layout(Size{Cols: s.geometry.outer.Dx(), Rows: s.geometry.outer.Dy()})
	if err != nil {
		return err
	}
	if g == s.geometry {
		return nil
	}
	return s.applyGeometry(g)
}

func (s *session[T]) applyGeometry(g geometry) error {
	if err := s.terminal.Resize(s.console.childSize(g)); err != nil {
		return err
	}
	ws := s.console.childWinsize(g)
	if err := unix.IoctlSetWinsize(s.fd, unix.TIOCSWINSZ, &unix.Winsize{Col: ws.Cols, Row: ws.Rows, Xpixel: ws.X, Ypixel: ws.Y}); err != nil {
		return err
	}
	s.geometry = g
	s.screen = s.console.outerScreen(g)
	s.composed = false
	s.console.renderer.Resize(g.outer.Dx(), g.outer.Dy())
	s.snapshot.Outer = Size{Cols: g.outer.Dx(), Rows: g.outer.Dy()}
	s.snapshot.Viewport = Size{Cols: g.child.Dx(), Rows: g.child.Dy()}
	if err := s.effects(); err != nil {
		return err
	}
	if err := s.refresh(true); err != nil {
		return err
	}
	return s.events.emit(Event{Kind: Resized, Snapshot: &s.snapshot})
}

func (s *session[T]) openWake() error {
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	s.wakeRead, s.wakeWrite = r, w
	s.wakeReadFD, s.wakeWriteFD = int(r.Fd()), int(w.Fd())
	for _, fd := range []int{s.wakeReadFD, s.wakeWriteFD} {
		if err := unix.SetNonblock(fd, true); err != nil {
			return errors.Join(err, s.closeWake())
		}
	}
	return nil
}

func (s *session[T]) closeWake() error {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	var errs []error
	errs = append(errs, s.wakeErr)
	if s.wakeRead != nil {
		errs = append(errs, s.wakeRead.Close())
		s.wakeRead = nil
	}
	if s.wakeWrite != nil {
		errs = append(errs, s.wakeWrite.Close())
		s.wakeWrite = nil
	}
	return errors.Join(errs...)
}

func (s *session[T]) wakeLoop() {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	if s.wakeWrite == nil {
		return
	}
	for {
		_, err := unix.Write(s.wakeWriteFD, []byte{1})
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err == nil || errors.Is(err, unix.EAGAIN) {
			// A full nonblocking pipe already contains a pending wakeup.
			return
		}
		// Closing the owned writer makes Poll report HUP on the reader.
		// Serialize all producers so none can write to a recycled descriptor.
		s.wakeErr = errors.Join(fmt.Errorf("wake terminal loop: %w", err), s.wakeWrite.Close())
		s.wakeWrite = nil
		return
	}
}

func (s *session[T]) wakeFailure() error {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	if s.wakeErr != nil {
		return s.wakeErr
	}
	return errors.New("terminal wake pipe disconnected")
}

func (s *session[T]) monitor(ctx context.Context, stop <-chan struct{}, done chan<- struct{}, resizes chan<- struct{}) {
	defer close(done)
	canceled := ctx.Done()
	for {
		select {
		case <-stop:
			return
		case <-canceled:
			canceled = nil
			s.wakeLoop()
		case <-s.frame.wake:
			s.wakeLoop()
		case <-s.winch:
			select {
			case resizes <- struct{}{}:
			default:
			}
			s.wakeLoop()
		}
	}
}

// signalChild signals process group pgid. A group that no longer exists, or
// whose members have all exited or are exiting, has nothing left to signal.
func signalChild(pgid int, sig syscall.Signal) error {
	err := unix.Kill(-pgid, sig)
	if err == nil || errors.Is(err, unix.ESRCH) {
		return nil
	}
	if errors.Is(err, unix.EPERM) {
		exited, recheckErr := deniedByExitedGroup(pgid)
		if exited {
			return nil
		}
		if recheckErr != nil {
			err = errors.Join(err, recheckErr)
		}
	}
	return fmt.Errorf("signal child process group: %w", err)
}

func (s *session[T]) signalGroups(sig syscall.Signal) error {
	groups, err := process.Groups(s.frame.cmd.Process.Pid)
	var errs []error
	if err != nil {
		errs = append(errs, err)
		if !s.waited {
			groups = append(groups, s.frame.cmd.Process.Pid)
		}
	}
	for _, pgid := range groups {
		errs = append(errs, signalChild(pgid, sig))
	}
	return errors.Join(errs...)
}

// kill sends SIGKILL to the owned session's process groups. The first kill
// before Wait reports the launch leader starts the reap deadline, which the
// loop and cleanupChild share. On Darwin an exiting session leader waits for
// its unread output to drain, and SIGKILL cannot end that wait. Output that
// flow control has stopped cannot be read, and while a process in another
// session holds the slave the wait has no deadline of its own. Closing the
// master ends it.
func (s *session[T]) kill() error {
	if !s.waited && s.reapDeadline.IsZero() {
		s.reapDeadline = time.Now().Add(drainTimeout)
	}
	return s.signalGroups(syscall.SIGKILL)
}

// reapOverdue reports whether the reap deadline passed before Wait reported
// the launch leader.
func (s *session[T]) reapOverdue() bool {
	return !s.waited && !s.reapDeadline.IsZero() && !time.Now().Before(s.reapDeadline)
}

// reap handles Wait's report of the launch leader while the session loop
// runs. The output drain that follows still draws, so the snapshot is sampled
// whether or not Exited is observed.
func (s *session[T]) reap() error {
	select {
	case wait := <-s.wait:
		err := s.recordReap(wait)
		s.queue = nil
		s.queuedBytes = 0
		s.collectMetadata()
		s.reapDeadline = time.Time{}
		s.drainDeadline = time.Now().Add(drainTimeout)
		// Held input can no longer reach the child. Route it for capture and
		// observers as its read would have; enqueue now discards its bytes.
		err = errors.Join(err, s.routeHeld())
		if s.statePending && s.events.wants(Exited) {
			err = errors.Join(err, s.refreshState())
		}
		// Routing that overflowed the queue leaves it full for Exited too.
		return joinOnce(err, s.emitExited())
	default:
		return nil
	}
}

// finishWait records Wait's report of the launch leader after the session loop
// has returned. Nothing draws any more, so the snapshot is sampled only for the
// Exited cleanupChild emits, and outer input modes stay as the loop left them
// for restoration.
func (s *session[T]) finishWait(err error) error {
	errs := []error{s.recordReap(err)}
	if s.reportsExit() {
		s.collectMetadata()
		if s.statePending {
			errs = append(errs, s.captureState())
		}
	}
	return errors.Join(errs...)
}

// emitExited emits Exited for the reaped launch leader once Run has emitted
// Started. A session that failed before Started emits neither.
func (s *session[T]) emitExited() error {
	if !s.started {
		return nil
	}
	return s.events.emit(Event{Kind: Exited, Snapshot: &s.snapshot})
}

// recordReap records Wait's report of the launch leader: the leader is reaped,
// and the snapshot carries the process state Run returns in Result. It returns
// Wait's error unless that only reports the leader's exit status.
func (s *session[T]) recordReap(err error) error {
	s.waited = true
	s.snapshot.Child.ProcessState = s.frame.cmd.ProcessState
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return fmt.Errorf("wait child: %w", err)
	}
	return nil
}

// reportsExit reports whether the launch leader's reap emits an observed
// Exited.
func (s *session[T]) reportsExit() bool {
	return s.started && s.events.wants(Exited)
}

// cleanupChild terminates the owned session and reaps the launch leader unless
// the loop has. For a leader it reaps, it emits Exited and returns the emit
// error apart from cleanup's own: a full queue is an observation failure, not
// a cleanup outcome.
func (s *session[T]) cleanupChild() (cleanupErr, exitedErr error) {
	reaps := !s.waited
	var errs []error
	// The leader can exit before its foreground or background jobs. Inventory
	// the owned native session even after Wait; detached sessions are excluded.
	errs = append(errs, s.kill())
	if !s.waited {
		errs = append(errs, s.discardUntilReaped())
	}
	// Closing the master hangs up the slave. Once the leader has been reaped its
	// exit has detached its session from the terminal, so the hangup signals no
	// one. A leader the drain could not release has been exiting since its
	// SIGKILL; the hangup releases it, and it discards the hangup's SIGHUP.
	if s.master != nil {
		if !s.waited && s.reportsExit() {
			// The leader's reap follows the close, which ends PTY sampling, so
			// Exited carries this last sample.
			s.collectPTY()
		}
		errs = append(errs, s.master.Close())
		// The closed descriptor's number can be reused by another file.
		s.master, s.fd = nil, -1
	}
	if !s.waited {
		errs = append(errs, s.finishWait(<-s.wait))
	}
	<-s.waitDone
	if reaps {
		exitedErr = s.emitExited()
	}
	return errors.Join(errs...), exitedErr
}

// discardUntilReaped reads and discards child output until Wait reports the
// killed launch leader or the reap deadline passes. Nothing else reads the
// master once the loop has returned, and on Darwin the exiting leader waits
// for its unread output to drain. That wait lasts 600 ms when the leader's
// close is the slave's last. Reading empties the queue and wakes the leader.
// Closing the master first would also end the wait, but its hangup sends the
// leader SIGHUP, which on Darwin can end a multi-threaded leader before its
// SIGKILL does and so change the exit status Run reports.
func (s *session[T]) discardUntilReaped() error {
	buf := make([]byte, readBufferSize)
	for {
		select {
		case err := <-s.wait:
			return s.finishWait(err)
		default:
		}
		timeout := int(time.Until(s.reapDeadline).Milliseconds())
		if timeout <= 0 {
			return nil
		}
		fds := []unix.PollFd{{Fd: int32(s.wakeReadFD), Events: unix.POLLIN}, {Fd: int32(s.fd), Events: unix.POLLIN}}
		if s.ptyEOF {
			fds[1].Fd = -1
		}
		if _, err := unix.Poll(fds, timeout); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return fmt.Errorf("poll child cleanup: %w", err)
		}
		if fds[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
			// Wait's wakeup can no longer arrive; closeWake reports why.
			return nil
		}
		if fds[0].Revents&unix.POLLIN != 0 {
			if err := s.clearWake(buf); err != nil {
				return err
			}
		}
		if fds[1].Revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) != 0 {
			n, err := unix.Read(s.fd, buf)
			if err := s.childReadError(n, err); err != nil {
				return err
			}
		}
	}
}

func (s *session[T]) loop(ctx context.Context) error {
	stop, done := make(chan struct{}), make(chan struct{})
	resizes := make(chan struct{}, 1)
	go s.monitor(ctx, stop, done, resizes)
	defer func() { close(stop); <-done }()
	buf := make([]byte, readBufferSize)
	var failure error
	for {
		if err := s.reap(); err != nil {
			failure = errors.Join(failure, err)
		}
		if err := ctx.Err(); err != nil && failure == nil {
			failure = context.Cause(ctx)
		}
		if s.waited && s.ptyEOF {
			// EOF may precede the last frame's deadline.
			return errors.Join(failure, s.render(false))
		}
		if err := s.frame.configurationError(); err != nil && failure == nil {
			failure = err
		}
		if failure != nil && !s.terminationStarted {
			s.frame.close()
			s.terminationStarted = true
			failure = errors.Join(failure, s.signalGroups(syscall.SIGTERM), s.signalGroups(syscall.SIGCONT))
			s.killDeadline = time.Now().Add(terminationTimeout)
		}
		if err := s.deadlines(); err != nil {
			return errors.Join(failure, err)
		}
		if s.reapOverdue() {
			// The killed leader's exit waits for output the loop cannot read.
			// cleanupChild closes the master, which releases it.
			return failure
		}
		disconnected, err := s.await(buf, resizes, failure)
		if err != nil {
			return errors.Join(failure, err)
		}
		if disconnected {
			failure = errors.New("outer terminal disconnected")
		}
	}
}

// pollFDs returns the wake pipe, outer terminal, child PTY and hangup watch
// descriptors with the events one loop iteration waits for.
func (s *session[T]) pollFDs(failure error) []unix.PollFd {
	fds := []unix.PollFd{{Fd: int32(s.wakeReadFD), Events: unix.POLLIN}, {Fd: int32(s.console.fd), Events: unix.POLLIN}, {Fd: int32(s.fd), Events: unix.POLLIN}, {Fd: -1}}
	if failure != nil || s.waited {
		fds[1].Fd = -1
	}
	if s.watchesHangup(failure) {
		// Paused input stays in the terminal; only its hangup is watched.
		fds[1].Events = 0
		fds[3] = s.hangup.pollFD()
	}
	if s.ptyEOF {
		fds[2].Fd = -1
	}
	if len(s.queue) > 0 && failure == nil {
		fds[2].Events |= unix.POLLOUT
	}
	return fds
}

// watchesHangup reports whether the loop watches the outer terminal only for
// its hangup: the session still accepts input, but held input pauses reads.
func (s *session[T]) watchesHangup(failure error) bool {
	return failure == nil && !s.waited && s.inputPaused()
}

// await waits once for the descriptors of pollFDs and handles every ready one.
// It reports whether the outer terminal disconnected.
func (s *session[T]) await(buf []byte, resizes <-chan struct{}, failure error) (bool, error) {
	hungUp, err := s.hangup.watch(s.console.fd, s.watchesHangup(failure))
	if err != nil || hungUp {
		return hungUp, err
	}
	fds := s.pollFDs(failure)
	if _, err := unix.Poll(fds, s.pollTimeout()); err != nil {
		if errors.Is(err, unix.EINTR) {
			return false, nil
		}
		return false, err
	}
	return s.processReady(buf, fds, resizes, failure == nil)
}

func (s *session[T]) processReady(buf []byte, fds []unix.PollFd, resizes <-chan struct{}, accepting bool) (bool, error) {
	if fds[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
		return false, s.wakeFailure()
	}
	if fds[0].Revents&unix.POLLIN != 0 {
		if err := s.handleWake(buf, resizes); err != nil {
			return false, err
		}
	}
	if fds[2].Revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) != 0 && !s.ptyEOF {
		if err := s.readChild(buf); err != nil {
			return false, err
		}
	}
	if fds[1].Revents&unix.POLLIN != 0 && accepting && !s.waited {
		if err := s.readOuter(buf); err != nil {
			return false, err
		}
		if err := s.render(false); err != nil {
			return false, err
		}
	}
	disconnected := fds[1].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 && accepting
	if fds[3].Revents != 0 && accepting {
		hungUp, err := s.hangup.hungUp()
		if err != nil {
			return disconnected, err
		}
		disconnected = disconnected || hungUp
	}
	if fds[2].Revents&unix.POLLOUT != 0 && accepting && !s.waited {
		if err := s.writeInput(); err != nil {
			return disconnected, err
		}
		routed, err := s.releaseHeld()
		if err != nil {
			return disconnected, err
		}
		// As after an outer read, mode changes from routed replies reach the
		// terminal now rather than at the next repaint.
		if routed {
			if err := s.render(false); err != nil {
				return disconnected, err
			}
		}
	}
	return disconnected, nil
}

func (s *session[T]) handleWake(buf []byte, resizes <-chan struct{}) error {
	if err := s.clearWake(buf); err != nil {
		return err
	}
	select {
	case <-resizes:
		if err := s.resize(); err != nil {
			return err
		}
	default:
	}
	if err := s.updateLayout(); err != nil {
		return err
	}
	return s.render(false)
}

// clearWake reads every pending wakeup so the next poll waits for a new one.
func (s *session[T]) clearWake(buf []byte) error {
	for {
		n, err := unix.Read(s.wakeReadFD, buf)
		if errors.Is(err, unix.EAGAIN) {
			return nil
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
	}
}

func (s *session[T]) deadlines() error {
	now := time.Now()
	if !s.renderDeadline.IsZero() && !now.Before(s.renderDeadline) {
		if err := s.render(false); err != nil {
			return err
		}
	}
	// While outer reads are paused, bytes completing an Escape may wait unread
	// in the terminal; releaseHeld restarts the deadline when reads resume.
	if !s.escapeDeadline.IsZero() && !now.Before(s.escapeDeadline) && !s.inputPaused() {
		s.escapeDeadline = time.Time{}
		if p, ok := s.framer.Flush(); ok {
			if err := s.hold(p); err != nil {
				return err
			}
		}
	}
	if !s.holdDeadline.IsZero() && !now.Before(s.holdDeadline) {
		s.holdDeadline = time.Time{}
		if err := s.terminal.ReleaseHold(); err != nil {
			return err
		}
		if err := s.effects(); err != nil {
			return err
		}
		if err := s.refresh(false); err != nil {
			return err
		}
	}
	if !s.killDeadline.IsZero() && !now.Before(s.killDeadline) {
		s.killDeadline = time.Time{}
		if err := s.kill(); err != nil {
			return err
		}
	}
	if !s.drainDeadline.IsZero() && !now.Before(s.drainDeadline) && !s.ptyEOF {
		s.drainErr = errors.New("child output did not close before drain deadline")
		return s.drainErr
	}
	return nil
}

func (s *session[T]) pollTimeout() int {
	escape := s.escapeDeadline
	if s.inputPaused() {
		escape = time.Time{} // deadlines defers the Escape until reads resume.
	}
	timeout := -1
	for _, d := range []time.Time{escape, s.holdDeadline, s.killDeadline, s.reapDeadline, s.drainDeadline, s.renderDeadline} {
		if !d.IsZero() {
			ms := max(0, int(time.Until(d).Milliseconds()+1))
			if timeout < 0 || ms < timeout {
				timeout = ms
			}
		}
	}
	return timeout
}
