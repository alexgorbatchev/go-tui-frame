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

// EncodeKey borrows a live native event and produces an owned wire sequence
// using the child's current modes, including the native modifyOtherKeys state.
// optionAsAlt states whether the event's Alt modifier is Alt or the macOS
// Option key that composed the event's text. Child state cannot express that,
// so the caller decides it per event; non-macOS encoders always treat Alt as Alt.
// A nil result means the requested child protocol produces no event.
func (t *Terminal) EncodeKey(event *ghostty.KeyEvent, optionAsAlt ghostty.OptionAsAlt) ([]byte, error) {
	if t == nil || t.native == nil {
		return nil, ErrClosed
	}
	if event == nil {
		return nil, errors.New("key event is nil")
	}
	if err := t.ensureKeyEncoder(); err != nil {
		return nil, err
	}
	t.keys.SetOptFromTerminal(t.native)
	// SetOptFromTerminal resets option-as-alt to false, so the caller's
	// decision must be applied after every load of the child's modes.
	t.keys.SetOptOptionAsAlt(optionAsAlt)
	result, err := t.keys.Encode(event)
	if err != nil {
		return nil, fmt.Errorf("encoding native key: %w", err)
	}
	return result, nil
}

func (t *Terminal) ensureKeyEncoder() error {
	if t.keys != nil {
		return nil
	}
	var err error
	t.keys, err = ghostty.NewKeyEncoder()
	if err != nil {
		return fmt.Errorf("creating native key encoder: %w", err)
	}
	return nil
}

func (t *Terminal) modifyOtherKeys2() (bool, error) {
	if err := t.ensureKeyEncoder(); err != nil {
		return false, err
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
	if t.mouse == nil {
		t.mouse, err = ghostty.NewMouseEncoder()
		if err != nil {
			return nil, fmt.Errorf("creating native mouse encoder: %w", err)
		}
		// Preserve every routed motion packet; native tracking mode still
		// decides whether the child requested that class of event.
		t.mouse.SetOptTrackLastCell(false)
	}
	t.mouse.SetOptFromTerminal(t.native)
	t.mouse.SetOptSize(geometry)
	t.mouse.SetOptAnyButtonPressed(anyButtonPressed)
	result, err := t.mouse.Encode(event)
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
