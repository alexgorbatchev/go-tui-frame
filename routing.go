package frame

import (
	"bytes"
	"fmt"
	"image"
	"maps"
	"runtime"
	"slices"
	"unicode"
	"unicode/utf8"
	"unsafe"

	"github.com/alexgorbatchev/go-tui-frame/internal/emulator"
	"github.com/alexgorbatchev/go-tui-frame/internal/input"
	uv "github.com/charmbracelet/ultraviolet"
	ghostty "go.mitchellh.com/libghostty"
)

// hostInputProfile contains modes actually applied by the console owner.
// Requested child modes alone do not establish what the outer terminal sends.
type hostInputProfile struct {
	KittyFlags                           ghostty.KittyKeyFlags
	ApplicationCursor, ApplicationKeypad bool
	Backarrow, Numlock                   bool
	AltEscPrefix, AltSendsEsc            bool
	ModifyOtherKeys2                     bool
	ModifyOtherKeysKnown                 bool
	MousePixels                          bool
	CellWidthPx, CellHeightPx            uint32
}

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
	key          *ghostty.KeyEvent
	keyText      string
	keyPin       runtime.Pinner
	mouse        *ghostty.MouseEvent
	mouseOwners  map[uv.MouseButton]bool
	pasting      bool
	pasteMarkers bool
}

func newInputRouter(terminal *emulator.Terminal, handler func(Input) Disposition) (*inputRouter, error) {
	if terminal == nil {
		return nil, fmt.Errorf("input router requires a native terminal")
	}
	r := &inputRouter{terminal: terminal, filter: keyboardFilter{handler: handler}, mouseOwners: make(map[uv.MouseButton]bool)}
	var err error
	r.key, err = ghostty.NewKeyEvent()
	if err != nil {
		return nil, fmt.Errorf("create native key event: %w", err)
	}
	r.mouse, err = ghostty.NewMouseEvent()
	if err != nil {
		r.close()
		return nil, fmt.Errorf("create native mouse event: %w", err)
	}
	return r, nil
}

func (r *inputRouter) close() {
	if r == nil {
		return
	}
	r.releaseKeyText()
	r.key.Close()
	r.mouse.Close()
	r.key, r.mouse = nil, nil
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
		return r.routeKey(packet, ev, state)
	default:
		// Unknown controls are not guessed to be keys or discarded as replies.
		result.Bytes = slices.Clone(packet.Raw)
		return result, nil
	}
}

// maxRoutedExpansion bounds the child-input bytes native encoding writes per
// byte of a converted key or mouse report. A one-byte upper-case letter is the
// worst case: UV reports it as Shift with its shifted text, and Kitty flags 4
// and 16 add the shifted alternate and associated text, so "Z" becomes
// "\x1b[122:90;2;90u". A six-byte X10 release names no button and becomes a
// release of each button the child owns: at most left, middle, right, back and
// forward, since wheel reports start no gesture and the native encoder rejects
// buttons 10 and 11. TestRoutedLimitBoundsNativeEncoding checks the bound
// against the native encoders.
const maxRoutedExpansion = 14

// routedLimit returns the most bytes route delivers for p. Only key and mouse
// reports are re-encoded; route forwards or drops every other packet.
func routedLimit(p input.Packet) int {
	switch p.Event.(type) {
	case uv.KeyEvent, uv.MouseEvent:
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

func (r *inputRouter) matchingKeys(packet input.Packet, state emulator.State) (bool, error) {
	cursor := r.host.ApplicationCursor
	if observed, ok := observedCursorMode(packet); ok {
		cursor = observed
	}
	keypad, numlock := r.host.ApplicationKeypad, r.host.Numlock
	if observedApplicationKeypad(packet) {
		// An actual SS3 keypad report proves effective application mode,
		// including that the outer terminal did not apply numeric mode 1035.
		keypad, numlock = true, false
	}
	modified := r.host.ModifyOtherKeys2
	if !r.host.ModifyOtherKeysKnown {
		// Native decoding already established that this is a keyboard event.
		// Its mode-2 wire form supplies provenance when outer queries cannot.
		modified = bytes.HasPrefix(packet.Raw, []byte("\x1b[27;")) || bytes.HasPrefix(packet.Raw, []byte("\x9b27;"))
	}
	if r.host.KittyFlags != state.KittyKeyboardFlags {
		return false, nil
	}
	// Native key encoding selects Kitty before its legacy modifyOtherKeys
	// path. A hidden legacy mode cannot require rewriting matching CSI-u.
	if state.KittyKeyboardFlags == 0 && modified != state.ModifyOtherKeys2 {
		return false, nil
	}
	for _, setting := range []struct {
		mode ghostty.Mode
		host bool
	}{
		{ghostty.ModeDECCKM, cursor},
		{ghostty.ModeKeypadKeys, keypad},
		{ghostty.ModeBackarrowKeyMode, r.host.Backarrow},
		{ghostty.ModeNumlockKeypad, numlock},
		{ghostty.ModeAltEscPrefix, r.host.AltEscPrefix},
		{ghostty.ModeAltSendsEsc, r.host.AltSendsEsc},
	} {
		child, err := routingMode(state, setting.mode)
		if err != nil {
			return false, err
		}
		if child != setting.host {
			return false, nil
		}
	}
	return true, nil
}

func observedCursorMode(packet input.Packet) (bool, bool) {
	event, ok := packet.Event.(uv.KeyEvent)
	if !ok {
		return false, false
	}
	switch event.Key().Code {
	case uv.KeyUp, uv.KeyDown, uv.KeyLeft, uv.KeyRight, uv.KeyHome, uv.KeyEnd:
	default:
		return false, false
	}
	// The native framer has already decoded the key. Its unmodified CSI/SS3
	// transport form identifies the cursor protocol even without a mode reply.
	// Parameterized and enhanced forms still use the negotiated profile.
	return observedFunctionMode(packet.Raw)
}

func observedApplicationKeypad(packet input.Packet) bool {
	event, ok := packet.Event.(uv.KeyEvent)
	if !ok || event.Key().Code < uv.KeyKpEnter || event.Key().Code > uv.KeyKp9 {
		return false
	}
	application, observed := observedFunctionMode(packet.Raw)
	return application && observed
}

func observedFunctionMode(raw []byte) (bool, bool) {
	if len(raw) == 3 && raw[0] == '\x1b' && (raw[1] == '[' || raw[1] == 'O') {
		return raw[1] == 'O', true
	}
	if len(raw) == 2 && (raw[0] == '\x8f' || raw[0] == '\x9b') {
		return raw[0] == '\x8f', true
	}
	return false, false
}

func (r *inputRouter) routeKey(packet input.Packet, event uv.KeyEvent, state emulator.State) (routedInput, error) {
	result := routedInput{Origin: "user-input"}
	r.filter.setReleases(r.host.KittyFlags&ghostty.KittyKeyReportEvents != 0)
	var err error
	result.Disposition, err = r.filter.handle(packet)
	if err != nil || result.Disposition == Consume {
		return result, err
	}
	agree, err := r.matchingKeys(packet, state)
	if err != nil {
		return result, err
	}
	if agree {
		result.Bytes = slices.Clone(packet.Raw)
		return result, nil
	}
	if err := r.setKey(event); err != nil {
		return result, err
	}
	defer r.releaseKeyText()
	result.Bytes, err = r.terminal.EncodeKey(r.key, optionAsAlt(event.Key()))
	result.Origin = "key-protocol"
	return result, err
}

// optionAsAlt reads the outer terminal's macOS Option decision from one key
// report. That terminal has already decided whether Option means Alt: an Alt
// key arrives with no text, or with the key's own or shifted character. With
// all keys and associated text reported, a terminal whose Option is not Alt
// still sets the Alt bit and attaches the text Option composed. Pinned Ghostty
// reports Option+b on a US layout as CSI 98;3;8747u under Kitty flags 25. The
// child must then receive that text.
func optionAsAlt(key uv.Key) ghostty.OptionAsAlt {
	if key.Mod.Contains(uv.ModAlt) && key.Text != "" && !isOwnKeyText(key) {
		return ghostty.OptionAsAltFalse
	}
	return ghostty.OptionAsAltTrue
}

// isOwnKeyText reports whether the key's text is its own or shifted character.
// Without a reported shifted key, Shift and Caps Lock select the upper case,
// as UV's Kitty decoder does when it derives text.
func isOwnKeyText(key uv.Key) bool {
	text, size := utf8.DecodeRuneInString(key.Text)
	if size != len(key.Text) {
		return false
	}
	if text == key.Code || key.ShiftedCode != 0 && text == key.ShiftedCode {
		return true
	}
	return key.ShiftedCode == 0 && key.Mod&(uv.ModShift|uv.ModCapsLock) != 0 && text == unicode.ToUpper(key.Code)
}

func nativeMods(mod uv.KeyMod) (ghostty.Mods, error) {
	const supported = uv.ModShift | uv.ModAlt | uv.ModCtrl | uv.ModSuper | uv.ModCapsLock | uv.ModNumLock
	if mod&^supported != 0 {
		return 0, fmt.Errorf("unsupported native modifier bits %#x", mod&^supported)
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

func (r *inputRouter) setKey(event uv.KeyEvent) error {
	key := event.Key()
	mods, err := nativeMods(key.Mod)
	if err != nil {
		return err
	}
	code, ok := functionalKey(key.Code)
	if !ok && key.Code > unicode.MaxRune && key.Code != uv.KeyExtended {
		return fmt.Errorf("unsupported native key code %d", key.Code)
	}
	// Printable logical keys do not identify a physical W3C key. Only an
	// explicitly reported PC-101 base code supplies that additional identity.
	if !ok && key.BaseCode != 0 {
		code = physicalBaseKey(key.BaseCode)
	}
	r.key.SetKey(code)
	r.key.SetMods(mods)
	r.key.SetConsumedMods(0)
	r.key.SetComposing(false)
	r.key.SetAction(ghostty.KeyActionPress)
	if _, release := event.(uv.KeyReleaseEvent); release {
		r.key.SetAction(ghostty.KeyActionRelease)
	} else if key.IsRepeat {
		r.key.SetAction(ghostty.KeyActionRepeat)
	}
	text, unshifted := key.Text, rune(0)
	if !ok && key.Code >= ' ' && key.Code <= unicode.MaxRune {
		unshifted = key.Code
		if text == "" {
			text = string(producedRune(key))
		}
	}
	if ok {
		text = "" // Native functional keys determine their own control text.
	}
	r.key.SetUnshiftedCodepoint(unshifted)
	// SetUTF8 borrows its string pointer across CGO calls. Pin the backing
	// storage while the native event retains it, not merely during the setter.
	r.keyText = text
	if text != "" {
		r.keyPin.Pin(unsafe.StringData(text))
	}
	r.key.SetUTF8(text)
	return nil
}

// producedRune returns the character a printable key produced when its report
// carries no text, as UV leaves it for any modifier beyond Shift. Native legacy
// and modifyOtherKeys encoders write that text, so the unshifted code would
// turn Alt+Shift+b into Alt+b. The outer terminal's reported shifted key comes
// first. Otherwise Shift or Caps Lock alone selects the upper case, and both
// together cancel, as in the xkb ALPHABETIC key type. Ctrl keeps the unshifted
// key: native ctrlSeq keeps Shift on A to Z, so the upper case would turn
// Ctrl+Shift+b from the C0 byte a legacy child expects into CSI u.
func producedRune(key uv.Key) rune {
	if key.Mod.Contains(uv.ModCtrl) {
		return key.Code
	}
	if key.ShiftedCode != 0 {
		return key.ShiftedCode
	}
	if key.Mod.Contains(uv.ModShift) != key.Mod.Contains(uv.ModCapsLock) {
		return unicode.ToUpper(key.Code)
	}
	return key.Code
}

func (r *inputRouter) releaseKeyText() {
	if r.key != nil {
		r.key.SetUTF8("")
	}
	if r.keyText != "" {
		r.keyPin.Unpin()
		r.keyText = ""
	}
}

func functionalKey(code rune) (ghostty.Key, bool) {
	key, ok := nativeFunctionalKeys[code]
	return key, ok
}

// BaseCode is explicitly defined by UV as PC-101 layout identity. Ordinary
// Unicode Code values must not go through this table as guessed physical keys.
func physicalBaseKey(code rune) ghostty.Key {
	if code >= 'a' && code <= 'z' {
		return ghostty.KeyA + ghostty.Key(code-'a')
	}
	if code >= '0' && code <= '9' {
		return ghostty.KeyDigit0 + ghostty.Key(code-'0')
	}
	return nativeBaseKeys[code]
}

var nativeBaseKeys = map[rune]ghostty.Key{
	'`': ghostty.KeyBackquote, '\\': ghostty.KeyBackslash, '[': ghostty.KeyBracketLeft,
	']': ghostty.KeyBracketRight, ',': ghostty.KeyComma, '=': ghostty.KeyEqual,
	'-': ghostty.KeyMinus, '.': ghostty.KeyPeriod, ';': ghostty.KeySemicolon,
	'/': ghostty.KeySlash, '\'': ghostty.KeyQuote,
}

var nativeFunctionalKeys = map[rune]ghostty.Key{
	uv.KeyEscape: ghostty.KeyEscape, uv.KeyEnter: ghostty.KeyEnter,
	uv.KeyTab: ghostty.KeyTab, uv.KeyBackspace: ghostty.KeyBackspace,
	uv.KeyUp: ghostty.KeyArrowUp, uv.KeyDown: ghostty.KeyArrowDown,
	uv.KeyLeft: ghostty.KeyArrowLeft, uv.KeyRight: ghostty.KeyArrowRight,
	uv.KeyInsert: ghostty.KeyInsert, uv.KeyDelete: ghostty.KeyDelete,
	uv.KeyHome: ghostty.KeyHome, uv.KeyEnd: ghostty.KeyEnd,
	uv.KeyPgUp: ghostty.KeyPageUp, uv.KeyPgDown: ghostty.KeyPageDown,
	uv.KeyKpEnter: ghostty.KeyNumpadEnter, uv.KeyKpEqual: ghostty.KeyNumpadEqual,
	uv.KeyKpMultiply: ghostty.KeyNumpadMultiply, uv.KeyKpPlus: ghostty.KeyNumpadAdd,
	uv.KeyKpComma: ghostty.KeyNumpadComma, uv.KeyKpMinus: ghostty.KeyNumpadSubtract,
	uv.KeyKpDecimal: ghostty.KeyNumpadDecimal, uv.KeyKpDivide: ghostty.KeyNumpadDivide,
	uv.KeyKpSep: ghostty.KeyNumpadSeparator,
	uv.KeyKp0:   ghostty.KeyNumpad0, uv.KeyKp1: ghostty.KeyNumpad1,
	uv.KeyKp2: ghostty.KeyNumpad2, uv.KeyKp3: ghostty.KeyNumpad3,
	uv.KeyKp4: ghostty.KeyNumpad4, uv.KeyKp5: ghostty.KeyNumpad5,
	uv.KeyKp6: ghostty.KeyNumpad6, uv.KeyKp7: ghostty.KeyNumpad7,
	uv.KeyKp8: ghostty.KeyNumpad8, uv.KeyKp9: ghostty.KeyNumpad9,
	uv.KeyKpUp: ghostty.KeyNumpadUp, uv.KeyKpDown: ghostty.KeyNumpadDown,
	uv.KeyKpLeft: ghostty.KeyNumpadLeft, uv.KeyKpRight: ghostty.KeyNumpadRight,
	uv.KeyKpHome: ghostty.KeyNumpadHome, uv.KeyKpEnd: ghostty.KeyNumpadEnd,
	uv.KeyKpPgUp: ghostty.KeyNumpadPageUp, uv.KeyKpPgDown: ghostty.KeyNumpadPageDown,
	uv.KeyKpInsert: ghostty.KeyNumpadInsert, uv.KeyKpDelete: ghostty.KeyNumpadDelete,
	uv.KeyKpBegin: ghostty.KeyNumpadBegin,
	uv.KeyF1:      ghostty.KeyF1, uv.KeyF2: ghostty.KeyF2, uv.KeyF3: ghostty.KeyF3,
	uv.KeyF4: ghostty.KeyF4, uv.KeyF5: ghostty.KeyF5, uv.KeyF6: ghostty.KeyF6,
	uv.KeyF7: ghostty.KeyF7, uv.KeyF8: ghostty.KeyF8, uv.KeyF9: ghostty.KeyF9,
	uv.KeyF10: ghostty.KeyF10, uv.KeyF11: ghostty.KeyF11, uv.KeyF12: ghostty.KeyF12,
	uv.KeyF13: ghostty.KeyF13, uv.KeyF14: ghostty.KeyF14, uv.KeyF15: ghostty.KeyF15,
	uv.KeyF16: ghostty.KeyF16, uv.KeyF17: ghostty.KeyF17, uv.KeyF18: ghostty.KeyF18,
	uv.KeyF19: ghostty.KeyF19, uv.KeyF20: ghostty.KeyF20, uv.KeyF21: ghostty.KeyF21,
	uv.KeyF22: ghostty.KeyF22, uv.KeyF23: ghostty.KeyF23, uv.KeyF24: ghostty.KeyF24,
	uv.KeyF25:      ghostty.KeyF25,
	uv.KeyCapsLock: ghostty.KeyCapsLock, uv.KeyNumLock: ghostty.KeyNumLock,
	uv.KeyScrollLock: ghostty.KeyScrollLock, uv.KeyPrintScreen: ghostty.KeyPrintScreen,
	uv.KeyPause: ghostty.KeyPause, uv.KeyMenu: ghostty.KeyContextMenu,
	uv.KeyMediaPlayPause: ghostty.KeyMediaPlayPause, uv.KeyMediaStop: ghostty.KeyMediaStop,
	uv.KeyMediaNext: ghostty.KeyMediaTrackNext, uv.KeyMediaPrev: ghostty.KeyMediaTrackPrevious,
	uv.KeyLowerVol: ghostty.KeyAudioVolumeDown, uv.KeyRaiseVol: ghostty.KeyAudioVolumeUp,
	uv.KeyMute:      ghostty.KeyAudioVolumeMute,
	uv.KeyLeftShift: ghostty.KeyShiftLeft, uv.KeyRightShift: ghostty.KeyShiftRight,
	uv.KeyLeftCtrl: ghostty.KeyControlLeft, uv.KeyRightCtrl: ghostty.KeyControlRight,
	uv.KeyLeftAlt: ghostty.KeyAltLeft, uv.KeyRightAlt: ghostty.KeyAltRight,
	uv.KeyLeftSuper: ghostty.KeyMetaLeft, uv.KeyRightSuper: ghostty.KeyMetaRight,
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
	if !state.MouseTracking {
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
		return nil, fmt.Errorf("native UTF-8 mouse encoding cannot represent this button event")
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
		return ghostty.MousePosition{}, inside, fmt.Errorf("child pixel mouse requires pixel input from the outer terminal")
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
		return fmt.Errorf("child mouse encoding cannot represent coordinates beyond %d", limit)
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
			return fmt.Errorf("unsupported native mouse button %d", m.Button)
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
		return fmt.Errorf("unsupported native mouse event %T", event)
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
