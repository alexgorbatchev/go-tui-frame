package emulator

import (
	"fmt"

	"github.com/charmbracelet/x/ansi"
	ghostty "go.mitchellh.com/libghostty"
)

// Profile holds only preferences actually reported by the outer terminal.
// Absent fields retain native defaults. Screen contents, screen transitions,
// mouse tracking, margins and clipboard permissions belong to each session.
type Profile struct {
	Foreground, Background, Cursor *ghostty.ColorRGB
	Palette                        map[uint8]ghostty.ColorRGB
	Modes                          map[ghostty.Mode]bool
	CursorStyle                    *ghostty.TerminalCursorStyle
	CursorBlink                    *bool
	Scheme                         *ghostty.ColorScheme
	KittyFlags                     *ghostty.KittyKeyFlags
	ModifyOtherKeys                *int
}

// PreferenceModes are the native resettable modes that describe user
// preferences supported by this endpoint, rather than per-application state.
// Grapheme clustering (mode 2027) is not one of them: Options.GraphemeWidth
// sets it, and an inherited default would replace both its value and its reset
// default.
func PreferenceModes() []ghostty.Mode {
	return []ghostty.Mode{
		ghostty.ModeKAM, ghostty.ModeInsert, ghostty.ModeSRM, ghostty.ModeLinefeed,
		ghostty.ModeDECCKM, ghostty.ModeSlowScroll, ghostty.ModeReverseColors,
		ghostty.ModeWraparound, ghostty.ModeAutorepeat, ghostty.ModeCursorVisible,
		ghostty.ModeReverseWrap, ghostty.ModeKeypadKeys, ghostty.ModeBackarrowKeyMode,
		ghostty.ModeFocusEvent, ghostty.ModeAltScroll, ghostty.ModeNumlockKeypad,
		ghostty.ModeAltEscPrefix, ghostty.ModeAltSendsEsc, ghostty.ModeReverseWrapExt,
		ghostty.ModeBracketedPaste, ghostty.ModeColorSchemeReport, ghostty.ModeInBandResize,
	}
}

func (t *Terminal) initializeProfile(p *Profile) error {
	// Native render state has concrete fallback colors even when the terminal
	// colors are unset. Seed both so OSC queries and partial host replies agree
	// with the colors the compositor actually paints.
	if err := t.render.Update(t.native); err != nil {
		return err
	}
	colors, err := t.render.Colors()
	if err != nil {
		return err
	}
	fg, bg := colors.Foreground, colors.Background
	if p != nil {
		if p.Foreground != nil {
			fg = *p.Foreground
			value := fg
			t.hostForeground = &value
		}
		if p.Background != nil {
			bg = *p.Background
			value := bg
			t.hostBackground = &value
		}
	}
	if err := t.native.SetColorForeground(&fg); err != nil {
		return fmt.Errorf("set default foreground: %w", err)
	}
	if err := t.native.SetColorBackground(&bg); err != nil {
		return fmt.Errorf("set default background: %w", err)
	}
	if p == nil {
		return nil
	}
	if err := t.native.SetColorCursor(p.Cursor); err != nil {
		return fmt.Errorf("set default cursor color: %w", err)
	}
	palette := colors.Palette
	for i, rgb := range p.Palette {
		palette[i] = rgb
		t.hostPalette[i], t.hostPaletteReported[i] = rgb, true
	}
	if err := t.native.SetColorPalette(&palette); err != nil {
		return fmt.Errorf("set default palette: %w", err)
	}
	for mode, value := range p.Modes {
		if err := t.native.SetModeDefault(mode, value); err != nil {
			return fmt.Errorf("set mode %d default: %w", mode.Value(), err)
		}
	}
	if p.Scheme != nil {
		scheme := *p.Scheme
		t.scheme = &scheme
		t.native.SetEffectColorScheme(func(_ *ghostty.Terminal) (ghostty.ColorScheme, bool) { return *t.scheme, true })
	}
	if err := t.native.SetDefaultCursorStyle(p.CursorStyle); err != nil {
		return fmt.Errorf("set default cursor style: %w", err)
	}
	if err := t.native.SetDefaultCursorBlink(p.CursorBlink); err != nil {
		return fmt.Errorf("set default cursor blink: %w", err)
	}
	if _, err := t.Write([]byte(ansi.SetCursorStyle(0))); err != nil {
		return err
	}
	if p.KittyFlags != nil {
		if _, err := t.Write([]byte(ansi.KittyKeyboard(int(*p.KittyFlags), 1))); err != nil {
			return err
		}
	}
	if p.ModifyOtherKeys != nil {
		if _, err := t.Write([]byte(fmt.Sprintf("\x1b[>4;%dm", *p.ModifyOtherKeys))); err != nil {
			return err
		}
	}
	return nil
}
