package frame

import (
	"errors"
	"fmt"
	"image"
	"maps"
	"slices"
	"unicode/utf8"

	"github.com/alexgorbatchev/go-tui-frame/v2/internal/emulator"
	"github.com/alexgorbatchev/go-tui-frame/v2/internal/input"
	uv "github.com/charmbracelet/ultraviolet"
	ghostty "go.mitchellh.com/libghostty"
)

// hostInputProfile contains modes actually applied by the console owner.
// Requested child modes alone do not establish what the outer terminal sends:
// its Kitty flags decide whether it reports key releases, and its pixel mouse
// mode and measured cells decide how its mouse reports map to child cells.
type hostInputProfile struct {
	KittyFlags                ghostty.KittyKeyFlags
	MousePixels               bool
	CellWidthPx, CellHeightPx uint32
}

// errUnroutable marks an input event the child's negotiated protocol cannot
// represent. The session withholds that one event and continues; every other
// routing error reports broken native or session state and ends the session.
var errUnroutable = errors.New("input event cannot be represented in the child's protocol")

type routedInput struct {
	Bytes       []byte
	Disposition Disposition
	Origin      string
}

// inputRouter shares the session's single owner with the native emulator.
// It does no I/O; delivered bytes go through the session's sole PTY writer.
type inputRouter struct {
	terminal     *emulator.Terminal
	filter       keyboardFilter
	viewport     image.Rectangle
	host         hostInputProfile
	mouse        *ghostty.MouseEvent
	mouseOwners  map[uv.MouseButton]bool
	pasting      bool
	pasteMarkers bool
}

func newInputRouter(terminal *emulator.Terminal, handler func(Input) Disposition) (*inputRouter, error) {
	if terminal == nil {
		return nil, fmt.Errorf("input router requires a native terminal")
	}
	mouse, err := ghostty.NewMouseEvent()
	if err != nil {
		return nil, fmt.Errorf("create native mouse event: %w", err)
	}
	return &inputRouter{terminal: terminal, filter: keyboardFilter{handler: handler}, mouse: mouse, mouseOwners: make(map[uv.MouseButton]bool)}, nil
}

func (r *inputRouter) close() {
	if r == nil {
		return
	}
	r.mouse.Close()
	r.mouse = nil
}

func (r *inputRouter) route(packet input.Packet, state emulator.State) (routedInput, error) {
	result := routedInput{Disposition: Pass, Origin: "user-input"}
	if packet.Paste {
		result.Bytes = slices.Clone(packet.Raw)
		return result, nil
	}
	switch ev := packet.Event.(type) {
	case uv.PasteStartEvent:
		bracketed, err := routingMode(state, ghostty.ModeBracketedPaste)
		if err != nil {
			return result, err
		}
		r.pasting, r.pasteMarkers = true, bracketed
		result.Origin = "paste-envelope"
		if bracketed {
			result.Bytes = slices.Clone(packet.Raw)
		}
		return result, nil
	case uv.PasteEndEvent:
		result.Origin = "paste-envelope"
		if r.pasting && r.pasteMarkers {
			result.Bytes = slices.Clone(packet.Raw)
		}
		r.pasting, r.pasteMarkers = false, false
		return result, nil
	case uv.FocusEvent, uv.BlurEvent:
		enabled, err := routingMode(state, ghostty.ModeFocusEvent)
		if err != nil {
			return result, err
		}
		result.Origin = "focus-input"
		if enabled {
			result.Bytes = slices.Clone(packet.Raw)
		}
		return result, nil
	case uv.MouseEvent:
		return r.routeMouse(ev, state)
	case uv.KeyEvent:
		return r.routeKey(packet)
	default:
		// Unknown controls are not guessed to be keys or discarded as replies.
		result.Bytes = slices.Clone(packet.Raw)
		return result, nil
	}
}

// maxRoutedExpansion bounds the child-input bytes native mouse encoding writes
// per byte of a mouse report. A six-byte X10 release is the worst case: it
// names no button and becomes a release of each button the child owns, at most
// left, middle, right, back and forward, since wheel reports start no gesture
// and the native encoder rejects buttons 10 and 11. With every modifier at the
// widest X10 coordinates, an SGR child receives 72 bytes, such as
// "\x1b[<156;221;220m" for the back button. TestRoutedLimitBoundsNativeEncoding
// checks the bound against the native encoders.
const maxRoutedExpansion = 12

// routedLimit returns the most bytes route delivers for p. Only mouse reports
// are re-encoded; route forwards or drops every other packet, keys included.
func routedLimit(p input.Packet) int {
	if _, mouse := p.Event.(uv.MouseEvent); mouse {
		return maxRoutedExpansion * len(p.Raw)
	}
	return len(p.Raw)
}

func routingMode(state emulator.State, mode ghostty.Mode) (bool, error) {
	if err := state.ModeErrors[mode]; err != nil {
		return false, fmt.Errorf("read child mode %d for routing: %w", mode.Value(), err)
	}
	value, ok := state.Modes[mode]
	if !ok {
		return false, fmt.Errorf("child mode %d is unavailable for routing", mode.Value())
	}
	return value, nil
}

// routeKey forwards a key the capture handler passes, or that arrives with no
// handler, exactly as the outer terminal sent it. The outer terminal runs the
// child's keyboard modes, so its bytes are already in the child's protocol.
func (r *inputRouter) routeKey(packet input.Packet) (routedInput, error) {
	result := routedInput{Origin: "user-input"}
	r.filter.setReleases(r.host.KittyFlags&ghostty.KittyKeyReportEvents != 0)
	var err error
	result.Disposition, err = r.filter.handle(packet)
	if err != nil || result.Disposition == Consume {
		return result, err
	}
	result.Bytes = slices.Clone(packet.Raw)
	return result, nil
}

func nativeMods(mod uv.KeyMod) (ghostty.Mods, error) {
	const supported = uv.ModShift | uv.ModAlt | uv.ModCtrl | uv.ModSuper | uv.ModCapsLock | uv.ModNumLock
	if mod&^supported != 0 {
		return 0, fmt.Errorf("%w: unsupported native modifier bits %#x", errUnroutable, mod&^supported)
	}
	var native ghostty.Mods
	for _, pair := range []struct {
		uv     uv.KeyMod
		native ghostty.Mods
	}{
		{uv.ModShift, ghostty.ModShift}, {uv.ModAlt, ghostty.ModAlt},
		{uv.ModCtrl, ghostty.ModCtrl}, {uv.ModSuper, ghostty.ModSuper},
		{uv.ModCapsLock, ghostty.ModCapsLock}, {uv.ModNumLock, ghostty.ModNumLock},
	} {
		if mod.Contains(pair.uv) {
			native |= pair.native
		}
	}
	return native, nil
}

func (r *inputRouter) routeMouse(event uv.MouseEvent, state emulator.State) (routedInput, error) {
	result := routedInput{Disposition: Pass, Origin: "mouse-protocol"}
	m := event.Mouse()
	position, inside, err := r.mousePosition(m, state)
	if err != nil {
		return result, err
	}
	_, press := event.(uv.MouseClickEvent)
	_, release := event.(uv.MouseReleaseEvent)
	owned := r.mouseOwners[m.Button]
	anonymous := release && m.Button == uv.MouseNone
	var released []uv.MouseButton
	switch {
	case anonymous:
		// Only SGR and SGR-pixel reports name the released button, using a
		// separate final character. X10 reports every release as button code
		// 3, which UV decodes as MouseNone, as it does an SGR release with
		// button code 3. That release ends every gesture, and the child
		// receives a release of each button it owns: an SGR child needs the
		// button to match the release to its press.
		released = r.childButtons()
		owned = len(released) > 0
		clear(r.mouseOwners)
	case release:
		delete(r.mouseOwners, m.Button)
	}
	if !inside {
		if press {
			r.mouseOwners[m.Button] = false
		}
		result.Origin = "outside-child"
		if r.unmeasuredPixels() {
			result.Origin = "unmeasured-pixel-mouse"
		}
		return result, nil
	}
	if (release || isMouseDrag(event)) && !owned {
		result.Origin = "unowned-mouse-gesture"
		return result, nil
	}
	if state.MouseTrackingMode == ghostty.MouseTrackingNone {
		result.Origin = "mouse-disabled"
		return result, nil
	}
	if err := mouseWireBounds(position, state); err != nil {
		return result, err
	}
	if anonymous {
		for _, button := range released {
			m.Button = button
			encoded, err := r.encodeMouse(uv.MouseReleaseEvent(m), position, state)
			if err != nil {
				result.Bytes = nil
				return result, err
			}
			result.Bytes = append(result.Bytes, encoded...)
		}
		return result, nil
	}
	if press {
		r.mouseOwners[m.Button] = true
	}
	result.Bytes, err = r.encodeMouse(event, position, state)
	if press && (err != nil || len(result.Bytes) == 0) {
		r.mouseOwners[m.Button] = false
	}
	return result, err
}

// childButtons returns the buttons the child owns, in button order. A press
// that names no button, as an SGR report with button code 3 does, is released
// with no button only when the child owns no named button: the named releases
// already end its gesture, and adding it to them would exceed routedLimit.
func (r *inputRouter) childButtons() []uv.MouseButton {
	var buttons []uv.MouseButton
	for _, button := range slices.Sorted(maps.Keys(r.mouseOwners)) {
		if button != uv.MouseNone && r.mouseOwners[button] {
			buttons = append(buttons, button)
		}
	}
	if len(buttons) == 0 && r.mouseOwners[uv.MouseNone] {
		buttons = append(buttons, uv.MouseNone)
	}
	return buttons
}

// encodeMouse writes event at position in the child's mouse encoding.
func (r *inputRouter) encodeMouse(event uv.MouseEvent, position ghostty.MousePosition, state emulator.State) ([]byte, error) {
	if err := r.setMouse(event, position); err != nil {
		return nil, err
	}
	pressed := false
	for _, childOwned := range r.mouseOwners {
		pressed = pressed || childOwned
	}
	encoded, err := r.terminal.EncodeMouse(r.mouse, pressed)
	if errors.Is(err, emulator.ErrMouseButton) {
		return nil, fmt.Errorf("%w: %w", errUnroutable, err)
	}
	if err != nil {
		return nil, err
	}
	if utf8.Valid(encoded) {
		return encoded, nil
	}
	encodedUTF8, err := routingMode(state, ghostty.ModeUTF8Mouse)
	if err != nil {
		return nil, err
	}
	if encodedUTF8 {
		// The pinned native UTF-8 encoder writes Cb as a raw byte. High
		// button codes cannot be forwarded as a valid UTF-8 report.
		return nil, fmt.Errorf("%w: native UTF-8 mouse encoding cannot represent this button event", errUnroutable)
	}
	return encoded, nil
}

func isMouseDrag(event uv.MouseEvent) bool {
	_, motion := event.(uv.MouseMotionEvent)
	return motion && event.Mouse().Button != uv.MouseNone
}

func (r *inputRouter) mousePosition(m uv.Mouse, state emulator.State) (ghostty.MousePosition, bool, error) {
	pixelChild, err := routingMode(state, ghostty.ModeSGRPixelsMouse)
	if err != nil {
		return ghostty.MousePosition{}, false, err
	}
	if r.viewport.Empty() {
		return ghostty.MousePosition{}, false, fmt.Errorf("mouse routing has no child viewport")
	}
	if r.unmeasuredPixels() {
		// The frame requests no mouse reports while it cannot place pixels in
		// cells, but reports the terminal sent earlier, such as before a resize
		// cleared the cell size, still arrive. They are outside every cell.
		return ghostty.MousePosition{}, false, nil
	}
	if r.host.MousePixels {
		if r.host.CellWidthPx != state.Size.CellWidthPx || r.host.CellHeightPx != state.Size.CellHeightPx {
			return ghostty.MousePosition{}, false, fmt.Errorf("pixel mouse input requires matching measured cell geometry")
		}
		// UV's SGR decoder subtracts one from wire coordinates even in pixel
		// mode. Bound the actual zero-based pixel, then restore the wire unit
		// for native SGR-pixel encoding, whose encoder emits pos without +1.
		w, h := int64(r.host.CellWidthPx), int64(r.host.CellHeightPx)
		x, y := int64(m.X)-int64(r.viewport.Min.X)*w, int64(m.Y)-int64(r.viewport.Min.Y)*h
		inside := x >= 0 && y >= 0 && x < int64(r.viewport.Dx())*w && y < int64(r.viewport.Dy())*h
		if pixelChild {
			x++
			y++
		}
		return ghostty.MousePosition{X: float32(x), Y: float32(y)}, inside, nil
	}
	inside := image.Pt(m.X, m.Y).In(r.viewport)
	if !inside {
		return ghostty.MousePosition{}, false, nil
	}
	if pixelChild {
		// The outer terminal reports cells: it lacks pixel support, or the
		// frame has not measured a cell size to enable it with.
		return ghostty.MousePosition{}, inside, fmt.Errorf("%w: child pixel mouse requires pixel input from the outer terminal", errUnroutable)
	}
	x, y := m.X-r.viewport.Min.X, m.Y-r.viewport.Min.Y
	w, h := state.Size.CellWidthPx, state.Size.CellHeightPx
	if w == 0 || h == 0 {
		// These are virtual grid units for cell encodings, not measured pixels.
		w, h = 1, 1
	}
	return ghostty.MousePosition{X: float32(x) * float32(w), Y: float32(y) * float32(h)}, inside, nil
}

// unmeasuredPixels reports whether the outer terminal sends pixel mouse
// reports while no cell size is measured to convert them.
func (r *inputRouter) unmeasuredPixels() bool {
	return r.host.MousePixels && (r.host.CellWidthPx == 0 || r.host.CellHeightPx == 0)
}

func mouseWireBounds(position ghostty.MousePosition, state emulator.State) error {
	for _, mode := range []ghostty.Mode{ghostty.ModeSGRPixelsMouse, ghostty.ModeSGRMouse, ghostty.ModeURxvtMouse} {
		enabled, err := routingMode(state, mode)
		if err != nil {
			return err
		}
		if enabled {
			return nil
		}
	}
	utf8Format, err := routingMode(state, ghostty.ModeUTF8Mouse)
	if err != nil {
		return err
	}
	const x10MaxPosition, utf8MaxPosition = 223, 2015
	limit := x10MaxPosition
	if utf8Format {
		limit = utf8MaxPosition
	}
	w, h := state.Size.CellWidthPx, state.Size.CellHeightPx
	if w == 0 || h == 0 {
		w, h = 1, 1
	}
	if position.X/float32(w) >= float32(limit) || position.Y/float32(h) >= float32(limit) {
		return fmt.Errorf("%w: child mouse encoding cannot represent coordinates beyond %d", errUnroutable, limit)
	}
	return nil
}

func (r *inputRouter) setMouse(event uv.MouseEvent, position ghostty.MousePosition) error {
	m := event.Mouse()
	mods, err := nativeMods(m.Mod)
	if err != nil {
		return err
	}
	r.mouse.SetMods(mods)
	r.mouse.SetPosition(position)
	r.mouse.ClearButton()
	if m.Button != uv.MouseNone {
		button, ok := nativeMouseButtons[m.Button]
		if !ok {
			return fmt.Errorf("%w: unsupported native mouse button %d", errUnroutable, m.Button)
		}
		r.mouse.SetButton(button)
	}
	switch event.(type) {
	case uv.MouseClickEvent, uv.MouseWheelEvent:
		r.mouse.SetAction(ghostty.MouseActionPress)
	case uv.MouseReleaseEvent:
		r.mouse.SetAction(ghostty.MouseActionRelease)
	case uv.MouseMotionEvent:
		r.mouse.SetAction(ghostty.MouseActionMotion)
	default:
		return fmt.Errorf("%w: unsupported native mouse event %T", errUnroutable, event)
	}
	return nil
}

var nativeMouseButtons = map[uv.MouseButton]ghostty.MouseButton{
	uv.MouseLeft: ghostty.MouseButtonLeft, uv.MouseMiddle: ghostty.MouseButtonMiddle,
	uv.MouseRight: ghostty.MouseButtonRight, uv.MouseWheelUp: ghostty.MouseButtonFour,
	uv.MouseWheelDown: ghostty.MouseButtonFive, uv.MouseWheelLeft: ghostty.MouseButtonSix,
	uv.MouseWheelRight: ghostty.MouseButtonSeven, uv.MouseBackward: ghostty.MouseButtonEight,
	uv.MouseForward: ghostty.MouseButtonNine, uv.MouseButton10: ghostty.MouseButtonTen,
	uv.MouseButton11: ghostty.MouseButtonEleven,
}
