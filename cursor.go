package frame

import (
	"fmt"

	"github.com/alexgorbatchev/go-tui-frame/internal/emulator"
	"github.com/charmbracelet/x/ansi"
	ghostty "go.mitchellh.com/libghostty"
)

type cursorAppearance struct {
	style *int
	color *ghostty.ColorRGB
}

func (c *console) syncCursor(s emulator.State) error {
	if c.preferences.cursorStyle != nil {
		style := cursorStyle(s.Cursor)
		if c.cursor.style == nil || *c.cursor.style != style {
			if _, err := c.renderer.WriteString(ansi.SetCursorStyle(style)); err != nil {
				return err
			}
			c.cursor.style = &style
		}
	}
	if c.preferences.profile.Cursor != nil {
		rgb := *c.preferences.profile.Cursor
		if s.Colors.CursorHasValue {
			rgb = s.Colors.Cursor
		}
		if c.cursor.color == nil || *c.cursor.color != rgb {
			if _, err := c.renderer.WriteString(ansi.SetCursorColor(colorSpec(rgb))); err != nil {
				return err
			}
			c.cursor.color = &rgb
		}
	}
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

func (c *console) restoreCursor() error {
	if c.cursor.style != nil && c.preferences.cursorStyle != nil {
		if _, err := c.renderer.WriteString(ansi.SetCursorStyle(*c.preferences.cursorStyle)); err != nil {
			return err
		}
	}
	if c.cursor.color != nil && c.preferences.profile.Cursor != nil {
		if _, err := c.renderer.WriteString(ansi.SetCursorColor(colorSpec(*c.preferences.profile.Cursor))); err != nil {
			return err
		}
	}
	return nil
}
