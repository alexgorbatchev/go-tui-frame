package frame

import (
	"fmt"

	"github.com/alexgorbatchev/go-tui-frame/internal/emulator"
	"github.com/charmbracelet/x/ansi"
	ghostty "go.mitchellh.com/libghostty"
)

// cursorAppearance records the cursor the frame shows on the outer terminal in
// place of the terminal's entry appearance. While styled or colored is false,
// the terminal shows its entry value for that attribute, and the frame writes
// nothing for it. Writing a reported value back would turn the terminal's own
// default into an explicit override that outlives the session: Ghostty, for
// one, then stops applying its configured and theme cursor.
type cursorAppearance struct {
	styled, colored bool
	style           int
	color           ghostty.ColorRGB
}

// syncCursor mirrors the child's cursor style and color for each attribute the
// outer terminal reported at entry. With inheritance on, the child starts from
// those reports, so a child that never changes its cursor causes no writes.
func (c *console) syncCursor(s emulator.State) error {
	if c.preferences.cursorStyle != nil {
		if err := c.showCursorStyle(cursorStyle(s.Cursor)); err != nil {
			return err
		}
	}
	if c.preferences.profile.Cursor != nil {
		rgb := *c.preferences.profile.Cursor
		if s.Colors.CursorHasValue {
			rgb = s.Colors.Cursor
		}
		return c.showCursorColor(rgb)
	}
	return nil
}

// showCursorStyle makes the outer terminal show the DECSCUSR style. The
// reported entry style is written only to undo a style the frame wrote.
func (c *console) showCursorStyle(style int) error {
	styled := style != *c.preferences.cursorStyle
	if styled == c.cursor.styled && (!styled || style == c.cursor.style) {
		return nil
	}
	if _, err := c.renderer.WriteString(ansi.SetCursorStyle(style)); err != nil {
		return err
	}
	c.cursor.styled, c.cursor.style = styled, style
	return nil
}

// showCursorColor makes the outer terminal show the cursor color rgb. The
// reported entry color is written only to undo a color the frame wrote.
func (c *console) showCursorColor(rgb ghostty.ColorRGB) error {
	colored := rgb != *c.preferences.profile.Cursor
	if colored == c.cursor.colored && (!colored || rgb == c.cursor.color) {
		return nil
	}
	if _, err := c.renderer.WriteString(ansi.SetCursorColor(colorSpec(rgb))); err != nil {
		return err
	}
	c.cursor.colored, c.cursor.color = colored, rgb
	return nil
}

func cursorStyle(cursor ghostty.RenderStateCursor) int {
	style := 2
	switch cursor.VisualStyle {
	case ghostty.CursorVisualStyleUnderline:
		style = 4
	case ghostty.CursorVisualStyleBar:
		style = 6
	}
	if cursor.Blinking {
		style--
	}
	return style
}

func colorSpec(rgb ghostty.ColorRGB) string {
	return fmt.Sprintf("#%02x%02x%02x", rgb.R, rgb.G, rgb.B)
}

// restoreCursor returns each cursor attribute the session changed to the
// outer terminal's entry appearance. Attributes still at their entry value
// are left alone.
func (c *console) restoreCursor() error {
	if c.cursor.styled {
		if err := c.showCursorStyle(*c.preferences.cursorStyle); err != nil {
			return err
		}
	}
	if c.cursor.colored {
		return c.showCursorColor(*c.preferences.profile.Cursor)
	}
	return nil
}
