package frame

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"os/signal"
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
)

// Run executes the child in a native PTY and owns one outer-terminal session.
// Child exit status is returned in Result; errors describe session operations.
func (f *Frame[T]) Run(ctx context.Context) (result Result, err error) {
	if err = f.begin(ctx); err != nil {
		return result, err
	}
	defer f.close()
	// An observer panic cancels the session as the caller's context does, so
	// it ends through the same child termination and terminal restoration.
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	events := newEventDispatcher(f.observedKinds, cancel, f.observe)
	defer func() {
		events.close()
		// A callback can panic while close drains admitted observations, after
		// the session loop stopped reading its context.
		if failure := events.failure(); failure != nil && !errors.Is(err, failure) {
			err = errors.Join(err, failure)
		}
	}()
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
	c.capture = f.capture != nil
	c.inherit = f.inheritTerminal
	defer func() {
		result.CleanupError = errors.Join(result.CleanupError, c.restore())
		f.close()
		err = errors.Join(err, result.CleanupError)
	}()
	saved, err := c.probe(ctx, events)
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
	s := &session[T]{frame: f, console: c, terminal: em, router: router, events: events, geometry: g, screen: c.outerScreen(g), framer: c.framer}
	if err = s.openWake(); err != nil {
		return result, err
	}
	defer func() { result.CleanupError = errors.Join(result.CleanupError, s.closeWake()) }()
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
		result.CleanupError = errors.Join(result.CleanupError, s.cleanupChild())
		result.ProcessState = f.cmd.ProcessState
		result.DrainError = s.drainErr
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
	for _, p := range saved {
		if err = s.route(p); err != nil {
			return result, err
		}
	}
	if s.framer.NeedsDeadline() {
		s.escapeDeadline = time.Now().Add(escapeTimeout)
	}
	return result, s.loop(ctx)
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
	metadataDirty                                             bool
	master                                                    *os.File
	fd                                                        int
	wakeRead, wakeWrite                                       *os.File
	wakeReadFD, wakeWriteFD                                   int
	wakeMu                                                    sync.Mutex
	wakeErr                                                   error
	wait                                                      chan error
	waitDone                                                  chan struct{}
	waited, ptyEOF                                            bool
	terminationStarted                                        bool
	waitErr, drainErr                                         error
	queue                                                     []pendingInput
	inputBuffers                                              [][]byte
	inputBufferBytes                                          int
	queuedBytes                                               int
	framer                                                    *input.Framer
	escapeDeadline, holdDeadline, killDeadline, drainDeadline time.Time
	renderDeadline                                            time.Time
}

func childEnvironment(env []string) []string {
	// The child talks to the virtual endpoint, whose supported identity is
	// independent of the physical terminal's vendor and graphics features.
	out := make([]string, 0, len(env)+2)
	for _, v := range env {
		name, _, _ := strings.Cut(v, "=")
		switch name {
		case "TERM", "COLORTERM", "TERM_PROGRAM", "TERM_PROGRAM_VERSION", "KITTY_WINDOW_ID", "GHOSTTY_RESOURCES_DIR", "GHOSTTY_BIN_DIR", "WEZTERM_PANE", "ITERM_SESSION_ID":
			continue
		}
		out = append(out, v)
	}
	return append(out, "TERM=xterm-256color", "COLORTERM=truecolor")
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

func (s *session[T]) refreshState() error {
	if err := s.terminal.UpdateState(&s.nextState); err != nil {
		return err
	}
	state := s.nextState
	s.metadataDirty = s.metadataDirty || regionMetadataChanged(s.state, state)
	s.nextState, s.state = s.state, state
	s.statePending = false
	s.snapshot.ObservedAt = time.Now()
	s.snapshot.Terminal = TerminalSnapshot{Title: state.Title, Directory: state.Directory, Size: Size{Cols: state.Size.Cols, Rows: state.Size.Rows}, Cells: state.Cells, Cursor: Cursor{X: int(state.Cursor.ViewportX), Y: int(state.Cursor.ViewportY), Visible: state.Cursor.Visible && state.Cursor.ViewportHasValue}, Alternate: state.Alternate, Native: state}
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
	input.MouseTracking = state.MouseTracking
	return s.syncInput()
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
	force = force || s.metadataDirty
	if force || s.frame.regionsDirty() {
		s.collectMetadata()
	}
	s.frame.compose(&s.screen, s.geometry, s.snapshot, repaintDamage{
		full: !s.composed, regions: force, rows: s.terminal.DirtyRows(),
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
	s.metadataDirty = false
	s.composed = true
	s.paintedCursor = cursor
	s.paintedNativeCursor = native.Cursor
	s.paintedCursorColor, s.paintedCursorColorSet = native.Colors.Cursor, native.Colors.CursorHasValue
	s.terminal.ClearDamage()
	s.renderDeadline = time.Time{}
	return nil
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
	s.router.host = hostInputProfile{KittyFlags: s.console.kittyFlags, ApplicationCursor: s.console.applied[1], ApplicationKeypad: s.console.applied[66], Backarrow: s.console.applied[67], Numlock: s.console.applied[1035], AltEscPrefix: s.console.applied[1036], AltSendsEsc: s.console.applied[1039], ModifyOtherKeysKnown: s.console.modifySupported, ModifyOtherKeys2: s.console.modifyLevel == 2, MousePixels: s.console.applied[1016], CellWidthPx: s.console.cellWidth, CellHeightPx: s.console.cellHeight}
	r, err := s.router.route(p, s.inputState)
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

func (s *session[T]) effects() error {
	for _, e := range s.terminal.Effects() {
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
	for _, p := range packets {
		if err := s.route(p); err != nil {
			return err
		}
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
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGWINCH)
	defer signal.Stop(signals)
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
		case <-signals:
			select {
			case resizes <- struct{}{}:
			default:
			}
			s.wakeLoop()
		}
	}
}

func signalChild(pid int, sig syscall.Signal) error {
	err := unix.Kill(-pid, sig)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("signal child process group: %w", err)
	}
	return nil
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

func (s *session[T]) reap() error {
	select {
	case err := <-s.wait:
		s.waited = true
		s.waitErr = err
		s.queue = nil
		s.queuedBytes = 0
		s.snapshot.Child.ProcessState = s.frame.cmd.ProcessState
		s.collectMetadata()
		s.drainDeadline = time.Now().Add(drainTimeout)
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			return fmt.Errorf("wait child: %w", err)
		}
		if s.statePending && s.events.wants(Exited) {
			if err := s.refreshState(); err != nil {
				return err
			}
		}
		return s.events.emit(Event{Kind: Exited, Snapshot: &s.snapshot})
	default:
		return nil
	}
}

func (s *session[T]) cleanupChild() error {
	var errs []error
	// The leader can exit before its foreground or background jobs. Inventory
	// the owned native session even after Wait; detached sessions are excluded.
	errs = append(errs, s.signalGroups(syscall.SIGKILL))
	if !s.waited {
		err := <-s.wait
		s.waited = true
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			errs = append(errs, err)
		}
	}
	if s.master != nil {
		errs = append(errs, s.master.Close())
		s.master = nil
	}
	<-s.waitDone
	return errors.Join(errs...)
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
		fds := []unix.PollFd{{Fd: int32(s.wakeReadFD), Events: unix.POLLIN}, {Fd: int32(s.console.fd), Events: unix.POLLIN}, {Fd: int32(s.fd), Events: unix.POLLIN}}
		if failure != nil || s.waited {
			fds[1].Fd = -1
		}
		if s.ptyEOF {
			fds[2].Fd = -1
		}
		if len(s.queue) > 0 && failure == nil {
			fds[2].Events |= unix.POLLOUT
		}
		if _, err := unix.Poll(fds, s.pollTimeout()); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return errors.Join(failure, err)
		}
		disconnected, err := s.processReady(buf, fds, resizes, failure == nil)
		if err != nil {
			return errors.Join(failure, err)
		}
		if disconnected {
			failure = errors.New("outer terminal disconnected")
		}
	}
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
	if fds[2].Revents&unix.POLLOUT != 0 && accepting && !s.waited {
		if err := s.writeInput(); err != nil {
			return disconnected, err
		}
	}
	return disconnected, nil
}

func (s *session[T]) handleWake(buf []byte, resizes <-chan struct{}) error {
	for {
		n, err := unix.Read(s.wakeReadFD, buf)
		if errors.Is(err, unix.EAGAIN) {
			break
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			break
		}
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

func (s *session[T]) deadlines() error {
	now := time.Now()
	if !s.renderDeadline.IsZero() && !now.Before(s.renderDeadline) {
		if err := s.render(false); err != nil {
			return err
		}
	}
	if !s.escapeDeadline.IsZero() && !now.Before(s.escapeDeadline) {
		s.escapeDeadline = time.Time{}
		if p, ok := s.framer.Flush(); ok {
			if err := s.route(p); err != nil {
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
		if err := s.signalGroups(syscall.SIGKILL); err != nil {
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
	timeout := -1
	for _, d := range []time.Time{s.escapeDeadline, s.holdDeadline, s.killDeadline, s.drainDeadline, s.renderDeadline} {
		if !d.IsZero() {
			ms := max(0, int(time.Until(d).Milliseconds()+1))
			if timeout < 0 || ms < timeout {
				timeout = ms
			}
		}
	}
	return timeout
}
