package frame

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/alexgorbatchev/go-tui-frame/internal/emulator"
	"github.com/alexgorbatchev/go-tui-frame/internal/input"
	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	ghostty "go.mitchellh.com/libghostty"
	"golang.org/x/sys/unix"
)

const capabilityTimeout = 300 * time.Millisecond

// alternateScrollMode (DEC mode 1007) makes a terminal send wheel steps as
// cursor keys while its alternate screen is active and no mouse tracking is set.
const alternateScrollMode ansi.DECMode = 1007

var consoleOwners = struct {
	sync.Mutex
	devices map[uint64]bool
}{devices: make(map[uint64]bool)}

// Only modes with an observed entry value are changed. The alternate screen is
// required because its prior contents cannot be reconstructed from a TTY.
var consoleModes = []ansi.DECMode{1, 5, 7, 9, 12, 25, 66, 67, 1000, 1001, 1002, 1003, 1004, 1005, 1006, alternateScrollMode, 1015, 1016, 1035, 1036, 1039, 1049, 2004, synchronizedOutputMode, 2027, 2031}

type console struct {
	inherit                                              bool
	preferences                                          terminalPreferences
	cursor                                               cursorAppearance
	input, output                                        *os.File
	fd                                                   int
	device                                               uint64
	raw                                                  *term.State
	capture                                              bool
	entry                                                map[ansi.DECMode]ansi.ModeSetting
	applied                                              map[ansi.DECMode]bool
	pending                                              map[ansi.DECMode]bool
	kittyPending, kittySupported, kittyPushed, alternate bool
	kittyFlags                                           ghostty.KittyKeyFlags
	modifyPending, modifySupported                       bool
	modifyEntry, modifyLevel                             int
	cellPending                                          bool
	cellWidth, cellHeight                                uint32
	renderer                                             *uv.TerminalRenderer
	colorProfile                                         colorprofile.Profile
	framer                                               *input.Framer
}

func inspectConsole(in, out *os.File) (int, uint64, *unix.Winsize, error) {
	fd, ofd := int(in.Fd()), int(out.Fd())
	if !term.IsTerminal(uintptr(fd)) || !term.IsTerminal(uintptr(ofd)) {
		return 0, 0, nil, errors.New("input and output must be terminals")
	}
	var a, b unix.Stat_t
	if err := unix.Fstat(fd, &a); err != nil {
		return 0, 0, nil, fmt.Errorf("stat input terminal: %w", err)
	}
	if err := unix.Fstat(ofd, &b); err != nil {
		return 0, 0, nil, fmt.Errorf("stat output terminal: %w", err)
	}
	if a.Rdev != b.Rdev {
		return 0, 0, nil, errors.New("input and output must refer to the same terminal")
	}
	w, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("read terminal dimensions: %w", err)
	}
	return fd, uint64(a.Rdev), w, nil
}

func acquireConsole(in, out *os.File, fd int, device uint64) (*console, error) {
	consoleOwners.Lock()
	if consoleOwners.devices[device] {
		consoleOwners.Unlock()
		return nil, errors.New("terminal is already owned by a frame session")
	}
	consoleOwners.devices[device] = true
	consoleOwners.Unlock()
	c := &console{input: in, output: out, fd: fd, device: device, inherit: true, entry: make(map[ansi.DECMode]ansi.ModeSetting), applied: make(map[ansi.DECMode]bool), pending: make(map[ansi.DECMode]bool)}
	var err error
	c.raw, err = term.MakeRaw(uintptr(fd))
	if err != nil {
		c.releaseOwner()
		return nil, fmt.Errorf("acquire raw terminal: %w", err)
	}
	c.attachRenderer(out, os.Environ())
	c.renderer.SetScrollOptim(false)
	c.renderer.SetTabStops(-1)
	return c, nil
}

// attachRenderer creates the outer renderer and records its color profile.
// The renderer detects the profile from the same output and environment but
// exposes no getter, so the detected value is set explicitly: the child
// encodes palette colors for exactly the profile the renderer downsamples to.
func (c *console) attachRenderer(out *os.File, env []string) {
	c.renderer = uv.NewTerminalRenderer(out, env)
	c.setColorProfile(colorprofile.Detect(out, env))
}

func (c *console) setColorProfile(p colorprofile.Profile) {
	c.colorProfile = p
	c.renderer.SetColorProfile(p)
}

func (c *console) releaseOwner() {
	consoleOwners.Lock()
	delete(consoleOwners.devices, c.device)
	consoleOwners.Unlock()
}

func (c *console) probe(ctx context.Context, events *eventDispatcher) (saved []input.Packet, err error) {
	var query string
	for _, m := range consoleModes {
		c.pending[m] = true
		query += ansi.RequestMode(m)
	}
	c.kittyPending = true
	c.modifyPending, c.cellPending = true, true
	query += ansi.RequestKittyKeyboard + ansi.QueryModifyOtherKeys + ansi.WindowOp(16)
	query += c.preferenceQueries()
	// Query replies can fill the input queue before all queries are written.
	// Poll both directions so neither endpoint waits on the other to drain.
	ofd := int(c.output.Fd())
	flags, err := unix.FcntlInt(uintptr(ofd), unix.F_GETFL, 0)
	if err != nil {
		return nil, fmt.Errorf("read probe descriptor flags: %w", err)
	}
	if err := unix.SetNonblock(ofd, true); err != nil {
		return nil, fmt.Errorf("set probe nonblocking: %w", err)
	}
	defer func() {
		_, restoreErr := unix.FcntlInt(uintptr(ofd), unix.F_SETFL, flags)
		if restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("restore probe descriptor flags: %w", restoreErr))
		}
	}()
	writes := []byte(query)
	deadline := time.Now().Add(capabilityTimeout)
	c.framer = input.New()
	buf := make([]byte, 4096)
	for time.Now().Before(deadline) && (len(writes) > 0 || len(c.pending) > 0 || c.kittyPending || c.modifyPending || c.cellPending || c.preferences.pending()) {
		if ctx.Err() != nil {
			return nil, context.Cause(ctx)
		}
		fds := []unix.PollFd{{Fd: int32(c.fd), Events: unix.POLLIN}, {Fd: -1}}
		if len(writes) > 0 {
			fds[1] = unix.PollFd{Fd: int32(ofd), Events: unix.POLLOUT}
		}
		if _, err := unix.Poll(fds, min(50, int(time.Until(deadline).Milliseconds()+1))); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return nil, err
		}
		if (fds[0].Revents|fds[1].Revents)&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
			return nil, errors.New("terminal closed during capability negotiation")
		}
		if fds[1].Revents&unix.POLLOUT != 0 {
			n, err := unix.Write(ofd, writes)
			if err != nil && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
				return nil, fmt.Errorf("write terminal probes: %w", err)
			}
			if n > 0 {
				writes = writes[n:]
			}
		}
		if fds[0].Revents&unix.POLLIN == 0 {
			continue
		}
		n, err := unix.Read(c.fd, buf)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
				continue
			}
			return nil, err
		}
		if n == 0 {
			return nil, errors.New("terminal input ended during negotiation")
		}
		if err := events.emit(Event{Kind: OuterInput, Bytes: buf[:n], Origin: "terminal-probe"}); err != nil {
			return nil, err
		}
		packets, err := c.framer.Feed(buf[:n])
		if err != nil {
			return nil, err
		}
		for _, p := range packets {
			if !c.consumeReply(p) {
				saved = append(saved, p)
			}
		}
	}
	if len(writes) > 0 {
		return nil, errors.New("terminal probe write timed out")
	}
	c.preferences.closed = true
	if v, ok := c.entry[1049]; !ok || !v.IsReset() {
		return nil, errors.New("terminal must report an inactive alternate screen (DEC mode 1049)")
	}
	return saved, nil
}

func (c *console) consumeReply(p input.Packet) bool {
	preference := c.consumePreferenceReply(p)
	switch ev := p.Event.(type) {
	case uv.ModeReportEvent:
		m, ok := ev.Mode.(ansi.DECMode)
		if !ok || !c.pending[m] {
			return preference
		}
		delete(c.pending, m)
		c.entry[m] = ev.Value
		// applied is the mode the outer terminal is in. A permanent value is
		// never switched, so it stays the effective mode for the session.
		if ev.Value.IsSet() || ev.Value.IsReset() {
			c.applied[m] = ev.Value.IsSet()
		}
		return true
	case uv.KeyboardEnhancementsEvent:
		if !c.kittyPending {
			return false
		}
		c.kittyPending = false
		c.kittySupported = true
		if c.inherit && !c.preferences.closed {
			flags := ghostty.KittyKeyFlags(ev.Flags)
			c.preferences.profile.KittyFlags = &flags
		}
		return true
	case uv.CellSizeEvent:
		// The child's native terminal answers the child's queries, so every
		// cell-size report from the outer terminal answers one the frame sent,
		// including duplicates of an earlier answer. Only a positive size is a
		// measurement; a terminal without one reports zeros.
		if ev.Width > 0 && ev.Height > 0 {
			c.cellPending = false
			c.cellWidth, c.cellHeight = uint32(ev.Width), uint32(ev.Height)
		}
		return true
	case uv.ModifyOtherKeysEvent:
		if !c.modifyPending || ev.Mode < 0 || ev.Mode > 2 {
			return false
		}
		c.modifyPending = false
		c.modifySupported = true
		c.modifyEntry, c.modifyLevel = ev.Mode, ev.Mode
		if c.inherit && !c.preferences.closed {
			level := ev.Mode
			c.preferences.profile.ModifyOtherKeys = &level
		}
		return true
	}
	return preference
}

func (c *console) enter() error {
	c.renderer.EnterAltScreen()
	c.alternate = true
	if err := c.writeEntryModes(); err != nil {
		return err
	}
	// Each outer frame owns its synchronized update and must release it before
	// waiting for input. Restore an observed entry hold only when leaving.
	if err := c.setMode(synchronizedOutputMode, false); err != nil {
		return err
	}
	if c.kittySupported {
		if _, err := c.renderer.WriteString(ansi.PushKittyKeyboard(0)); err != nil {
			return err
		}
		c.kittyPushed = true
	}
	if err := c.setMode(7, true); err != nil {
		return err
	}
	// The native cell colors already account for reverse-video mode.
	if err := c.setMode(5, false); err != nil {
		return err
	}
	grapheme := c.graphemeWidth()
	if err := c.setMode(2027, grapheme); err != nil {
		return err
	}
	c.renderer.SetGraphemeWidth(grapheme)
	if err := c.setMode(2004, true); err != nil {
		return err
	}
	return c.renderer.Flush()
}

// switchable reports whether the outer terminal reported m as set or reset
// (DECRPM Ps 1 or 2). A terminal ignores changes to a permanently set or reset
// mode, and synchronized output needs both transitions to delimit frames.
func (c *console) switchable(m ansi.DECMode) bool {
	v, ok := c.entry[m]
	return ok && (v == ansi.ModeSet || v == ansi.ModeReset)
}

// graphemeWidth reports whether the outer terminal measures text by grapheme
// clusters (mode 2027) for the session: enter turns a switchable mode on, and a
// permanently set mode is already on. It holds before enter, so the child
// emulator and the outer screen created earlier agree with the terminal.
func (c *console) graphemeWidth() bool {
	return c.switchable(2027) || c.entry[2027] == ansi.ModePermanentlySet
}

func (c *console) setMode(m ansi.DECMode, on bool) error {
	if !c.switchable(m) || c.applied[m] == on {
		return nil
	}
	var text string
	if m == synchronizedOutputMode {
		text = ansi.ResetModeSynchronizedOutput
		if on {
			text = ansi.SetModeSynchronizedOutput
		}
	} else if on {
		text = ansi.SetMode(m)
	} else {
		text = ansi.ResetMode(m)
	}
	if _, err := c.renderer.WriteString(text); err != nil {
		return err
	}
	c.applied[m] = on
	return nil
}

func (c *console) syncInput(s emulator.State) error {
	measured := c.cellWidth > 0 && c.cellHeight > 0
	// A terminal that keeps SGR pixel reports (1016) permanently on sends
	// coordinates the router can localize only with a measured cell size, so
	// mouse tracking stays off until one arrives. A cell-size reply or resize
	// runs applyGeometry, which synchronizes these modes again.
	localizable := measured || c.entry[1016] != ansi.ModePermanentlySet
	mirror := []struct {
		host     ansi.DECMode
		child    ghostty.Mode
		tracking bool
	}{
		{1, ghostty.ModeDECCKM, false}, {66, ghostty.ModeKeypadKeys, false}, {67, ghostty.ModeBackarrowKeyMode, false},
		{1035, ghostty.ModeNumlockKeypad, false}, {1036, ghostty.ModeAltEscPrefix, false}, {1039, ghostty.ModeAltSendsEsc, false},
		{9, ghostty.ModeX10Mouse, true}, {1000, ghostty.ModeNormalMouse, true}, {1002, ghostty.ModeButtonMouse, true}, {1003, ghostty.ModeAnyMouse, true},
		{1004, ghostty.ModeFocusEvent, false},
	}
	for _, m := range mirror {
		if err := c.setMode(m.host, s.Modes[m.child] && (localizable || !m.tracking)); err != nil {
			return err
		}
	}
	pixels := s.Modes[ghostty.ModeSGRPixelsMouse] && measured && c.switchable(1016)
	for _, m := range []ansi.DECMode{1001, 1005, 1015} {
		if err := c.setMode(m, false); err != nil {
			return err
		}
	}
	if err := c.setMode(1006, s.MouseTracking && !pixels); err != nil {
		return err
	}
	if err := c.setMode(1016, pixels && s.MouseTracking); err != nil {
		return err
	}
	// The outer screen is always alternate during a session, so the outer
	// terminal would turn wheel steps into cursor keys for any child without
	// mouse tracking. Convert only where the child itself would: on its
	// alternate screen with alternate scroll set and no mouse tracking. Child
	// tracking turns conversion off even while outer tracking is withheld.
	if err := c.setMode(alternateScrollMode, s.Alternate && s.Modes[ghostty.ModeAltScroll] && !s.MouseTracking); err != nil {
		return err
	}
	flags := s.KittyKeyboardFlags
	if c.capture {
		flags |= ghostty.KittyKeyDisambiguate
	}
	if !c.kittySupported {
		flags = 0
	}
	if flags != c.kittyFlags {
		if _, err := c.renderer.WriteString(ansi.KittyKeyboard(int(flags), 1)); err != nil {
			return err
		}
		c.kittyFlags = flags
	}
	if c.modifySupported {
		level := 0
		if s.ModifyOtherKeys2 || c.capture && !c.kittySupported {
			level = 2
		}
		if err := c.setModify(level); err != nil {
			return err
		}
	}
	return nil
}

func (c *console) setModify(level int) error {
	if level == c.modifyLevel {
		return nil
	}
	if _, err := c.renderer.WriteString(fmt.Sprintf("\x1b[>4;%dm", level)); err != nil {
		return err
	}
	c.modifyLevel = level
	return nil
}

func (c *console) writeEntryModes() error {
	for _, m := range consoleModes {
		if m == 1049 || !c.switchable(m) {
			continue
		}
		on := c.entry[m] == ansi.ModeSet
		text := ansi.ResetMode(m)
		if on {
			text = ansi.SetMode(m)
		}
		if _, err := c.renderer.WriteString(text); err != nil {
			return err
		}
		c.applied[m] = on
	}
	return nil
}

func (c *console) restore() error {
	defer c.releaseOwner()
	var errs []error
	if c.renderer != nil {
		if c.kittyPushed {
			if _, err := c.renderer.WriteString(ansi.PopKittyKeyboard(1)); err != nil {
				errs = append(errs, err)
			}
		}
		if c.alternate {
			c.renderer.ExitAltScreen()
		}
		if err := c.writeEntryModes(); err != nil {
			errs = append(errs, err)
		}
		if err := c.restoreCursor(); err != nil {
			errs = append(errs, err)
		}
		if c.modifySupported {
			if err := c.setModify(c.modifyEntry); err != nil {
				errs = append(errs, err)
			}
		}
		if err := c.renderer.Flush(); err != nil {
			errs = append(errs, fmt.Errorf("restore terminal modes: %w", err))
		}
	}
	if c.raw != nil {
		if err := term.Restore(uintptr(c.fd), c.raw); err != nil {
			errs = append(errs, fmt.Errorf("restore termios: %w", err))
		}
	}
	return errors.Join(errs...)
}
