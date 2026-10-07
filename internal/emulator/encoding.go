package emulator

import (
	"errors"
	"fmt"

	ghostty "go.mitchellh.com/libghostty"
)

const maxPixelDimension = 1<<32 - 1

var (
	ErrPixelGeometry = errors.New("SGR pixel mouse requires measured cell dimensions")
	ErrMouseButton   = errors.New("native mouse encoder does not encode this button")
)

// modifyOtherKeys2 reads whether the child enabled modifyOtherKeys mode 2 by
// encoding a probe key with the native key encoder.
func (t *Terminal) modifyOtherKeys2() (bool, error) {
	if t.keys == nil {
		var err error
		t.keys, err = ghostty.NewKeyEncoder()
		if err != nil {
			return false, fmt.Errorf("creating native key encoder: %w", err)
		}
	}
	if t.probe == nil {
		var err error
		t.probe, err = ghostty.NewKeyEvent()
		if err != nil {
			return false, fmt.Errorf("creating native keyboard-state probe: %w", err)
		}
		t.probe.SetAction(ghostty.KeyActionPress)
		t.probe.SetKey(ghostty.KeyA)
		t.probe.SetMods(ghostty.ModCtrl)
		t.probe.SetUTF8("a")
		t.probe.SetUnshiftedCodepoint('a')
	}
	// Native key_encode.zig handles mode 2 before Ctrl-to-C0 conversion.
	// Disabling Kitty on this encoder (not the terminal) exposes that behavior
	// even when a child enabled both protocols. Nothing is written to the PTY.
	t.keys.SetOptFromTerminal(t.native)
	t.keys.SetOptKittyFlags(ghostty.KittyKeyDisabled)
	encoded, err := t.keys.Encode(t.probe)
	if err != nil {
		return false, fmt.Errorf("observing native keyboard format: %w", err)
	}
	switch string(encoded) {
	case "\x01":
		return false, nil
	case "\x1b[27;5;97~":
		return true, nil
	default:
		return false, fmt.Errorf("native keyboard format probe returned unexpected %q", encoded)
	}
}

// mouseTrackingProbes classify the child's tracking mode by the first event
// the native encoder does not report (mouse_encode.zig shouldReport). Each
// mode reports every event of the modes before it.
var mouseTrackingProbes = []struct {
	action ghostty.MouseAction
	held   bool
	// unreported is the active mode when the encoder drops this event.
	unreported ghostty.MouseTrackingMode
}{
	{ghostty.MouseActionPress, true, ghostty.MouseTrackingNone},     // every mode reports a left press
	{ghostty.MouseActionRelease, true, ghostty.MouseTrackingX10},    // X10 reports presses only
	{ghostty.MouseActionMotion, true, ghostty.MouseTrackingNormal},  // normal mode reports no motion
	{ghostty.MouseActionMotion, false, ghostty.MouseTrackingButton}, // button mode needs a held button
}

// mouseTrackingMode reads the child's active mouse tracking mode by encoding
// probe events with the native mouse encoder. Ghostty keeps that mode apart
// from the DEC mode bits, and the pinned getter API reports only whether any
// of the bits is set. Nothing is written to the PTY.
func (t *Terminal) mouseTrackingMode() (ghostty.MouseTrackingMode, error) {
	encoder, err := t.mouseEncoder()
	if err != nil {
		return ghostty.MouseTrackingNone, err
	}
	if t.mouseProbe == nil {
		t.mouseProbe, err = ghostty.NewMouseEvent()
		if err != nil {
			return ghostty.MouseTrackingNone, fmt.Errorf("creating native mouse-state probe: %w", err)
		}
	}
	// Only the tracking mode decides whether these events are reported. SGR
	// encodes every probe, and a one-cell surface keeps the probe inside the
	// viewport whatever the child's format and measured geometry.
	encoder.SetOptFromTerminal(t.native)
	encoder.SetOptFormat(ghostty.MouseFormatSGR)
	encoder.SetOptSize(ghostty.MouseEncoderSize{ScreenWidth: 1, ScreenHeight: 1, CellWidth: 1, CellHeight: 1})
	encoder.SetOptAnyButtonPressed(true)
	for _, probe := range mouseTrackingProbes {
		t.mouseProbe.SetAction(probe.action)
		if probe.held {
			t.mouseProbe.SetButton(ghostty.MouseButtonLeft)
		} else {
			t.mouseProbe.ClearButton()
		}
		encoded, err := encoder.Encode(t.mouseProbe)
		if err != nil {
			return ghostty.MouseTrackingNone, fmt.Errorf("observing native mouse tracking mode: %w", err)
		}
		if len(encoded) == 0 {
			return probe.unreported, nil
		}
	}
	return ghostty.MouseTrackingAny, nil
}

// mouseEncoder returns the terminal's native mouse encoder. Every caller
// configures it from the terminal before encoding.
func (t *Terminal) mouseEncoder() (*ghostty.MouseEncoder, error) {
	if t.mouse == nil {
		var err error
		t.mouse, err = ghostty.NewMouseEncoder()
		if err != nil {
			return nil, fmt.Errorf("creating native mouse encoder: %w", err)
		}
		// Preserve every routed motion packet; native tracking mode still
		// decides whether the child requested that class of event.
		t.mouse.SetOptTrackLastCell(false)
	}
	return t.mouse, nil
}

// EncodeMouse borrows a live event with child-local surface coordinates. With
// measured pixels those coordinates are pixels; otherwise they are unit cells.
// Unit geometry is never substituted for SGR pixel reporting. The router owns
// the set of buttons forwarded to the child and supplies its current state.
func (t *Terminal) EncodeMouse(event *ghostty.MouseEvent, anyButtonPressed bool) ([]byte, error) {
	if t == nil || t.native == nil {
		return nil, ErrClosed
	}
	if event == nil {
		return nil, errors.New("mouse event is nil")
	}
	if button, ok := event.Button(); ok && (button == ghostty.MouseButtonUnknown || button == ghostty.MouseButtonTen || button == ghostty.MouseButtonEleven) {
		return nil, ErrMouseButton
	}
	geometry, err := t.mouseGeometry()
	if err != nil {
		return nil, err
	}
	encoder, err := t.mouseEncoder()
	if err != nil {
		return nil, err
	}
	encoder.SetOptFromTerminal(t.native)
	encoder.SetOptSize(geometry)
	encoder.SetOptAnyButtonPressed(anyButtonPressed)
	result, err := encoder.Encode(event)
	if err != nil {
		return nil, fmt.Errorf("encoding native mouse: %w", err)
	}
	return result, nil
}

func (t *Terminal) mouseGeometry() (ghostty.MouseEncoderSize, error) {
	width, height := t.size.CellWidthPx, t.size.CellHeightPx
	if width == 0 || height == 0 {
		pixel, err := t.native.Mode(ghostty.ModeSGRPixelsMouse)
		if err != nil {
			return ghostty.MouseEncoderSize{}, fmt.Errorf("reading native mouse format: %w", err)
		}
		if pixel {
			return ghostty.MouseEncoderSize{}, ErrPixelGeometry
		}
		width, height = 1, 1
	}
	screenWidth := uint64(width) * uint64(t.size.Cols)
	screenHeight := uint64(height) * uint64(t.size.Rows)
	if screenWidth > maxPixelDimension || screenHeight > maxPixelDimension {
		return ghostty.MouseEncoderSize{}, errors.New("mouse surface exceeds native pixel dimensions")
	}
	return ghostty.MouseEncoderSize{
		ScreenWidth: uint32(screenWidth), ScreenHeight: uint32(screenHeight), CellWidth: width, CellHeight: height,
	}, nil
}
