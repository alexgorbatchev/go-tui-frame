package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"
	frame "github.com/alexgorbatchev/go-tui-frame"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

const (
	headerRows      = 3
	footerRows      = 2
	demoCount       = 3
	backgroundCount = 3
	red             = "#B91C1C"
	navy            = "#172554"
	teal            = "#115E59"
	white           = "#FFFFFF"
	slate           = "#0F172A"
	keyHints        = "Ctrl+B 1 layout | Ctrl+B 2 colour | Ctrl+B 3 border | Ctrl+Q quit"
)

// UIData is an immutable value published to each frame region.
type UIData struct {
	Demo       int
	Background int
	Border     bool
	Agent      bool
}

func demoName(demo int) string {
	return [...]string{"Signal bar", "Layered badge", "Bordered card"}[demo]
}

func drawHeader(ctx frame.DrawContext[UIData]) {
	view := ctx.View
	if view.Bounds().Empty() {
		return
	}
	text := fmt.Sprintf("%s | PID %d", demoName(ctx.Data.Demo), ctx.Term.Child.PID)
	if ctx.Data.Agent {
		paint(view, lipgloss.NewStyle(), text)
		return
	}
	style := headerStyle(ctx.Data.Background)
	paint(view, style, "")
	switch ctx.Data.Demo {
	case 0:
		paint(view, style.Padding(0, 1).AlignVertical(lipgloss.Center), text)
	case 1:
		title := lipgloss.NewLayer(style.Render(text)).Y(1)
		badgeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(navy)).
			Background(lipgloss.Color(white)).Bold(true).Padding(0, 1)
		badge := lipgloss.NewLayer(badgeStyle.Render("Ctrl+B 1 layout")).Z(1)
		badge.X(max(0, view.Bounds().Dx()-badge.Width()-1))
		lipgloss.NewCompositor(title, badge).Draw(view, view.Bounds())
	case 2:
		cardStyle := style.Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color(white)).BorderBackground(style.GetBackground()).Padding(0, 1).
			Width(max(1, view.Bounds().Dx()-2)).MaxWidth(view.Bounds().Dx()).MaxHeight(view.Bounds().Dy())
		card := lipgloss.NewLayer(cardStyle.Render(text)).X(1)
		lipgloss.NewCompositor(card).Draw(view, view.Bounds())
	}
}

func headerStyle(background int) lipgloss.Style {
	bg := [...]string{red, navy, teal}[background]
	return lipgloss.NewStyle().Background(lipgloss.Color(bg)).
		Foreground(lipgloss.Color(white)).Bold(true)
}

func drawFooter(ctx frame.DrawContext[UIData]) {
	if ctx.View.Bounds().Empty() {
		return
	}
	name := "starting"
	if ctx.Term.Child.Executable != "" {
		name = metadataText(filepath.Base(ctx.Term.Child.Executable))
	}
	border := "off"
	if ctx.Data.Border {
		border = "on"
	}
	metadata := fmt.Sprintf("%s | %d×%d | border %s | %s",
		name, ctx.Term.Viewport.Cols, ctx.Term.Viewport.Rows, border, metadataText(ctx.Term.Terminal.Title))
	if ctx.Data.Agent {
		paintFooter(ctx.View, lipgloss.NewStyle(), lipgloss.NewStyle(), metadata)
		return
	}
	bg := lipgloss.Color([...]string{slate, navy, teal}[ctx.Data.Demo])
	style := lipgloss.NewStyle().Background(bg).Foreground(lipgloss.Color(white))
	hintStyle := style
	switch ctx.Data.Demo {
	case 1:
		hintStyle = style.Bold(true)
	case 2:
		style = style.Border(lipgloss.NormalBorder(), false, false, false, true).
			BorderForeground(lipgloss.Color(white)).BorderBackground(bg)
		hintStyle = style
	}
	paintFooter(ctx.View, style, hintStyle, metadata)
}

// paintFooter paints the metadata row above the key-hint row. Lip Gloss wraps
// a block at its width before it applies a height limit, so each row is
// rendered on its own and cut to the cells Render leaves inside the row's
// border and padding: a long child title ends in an ellipsis instead of
// wrapping over the key hints.
func paintFooter(view uv.Screen, style, hintStyle lipgloss.Style, metadata string) {
	paint(view, style, "")
	width := view.Bounds().Dx()
	row := func(rowStyle lipgloss.Style, text string) *lipgloss.Layer {
		budget := width - rowStyle.GetHorizontalBorderSize() - rowStyle.GetHorizontalPadding()
		text = ansi.Truncate(text, budget, "…")
		return lipgloss.NewLayer(rowStyle.Width(width).Render(text))
	}
	metadataRow := row(style, metadata)
	hintRow := row(hintStyle, keyHints).Y(1)
	lipgloss.NewCompositor(metadataRow, hintRow).Draw(view, view.Bounds())
}

func paint(view uv.Screen, style lipgloss.Style, text string) {
	if view.Bounds().Empty() {
		return
	}
	style = style.Width(view.Bounds().Dx()).Height(view.Bounds().Dy()).
		MaxWidth(view.Bounds().Dx()).MaxHeight(view.Bounds().Dy())
	lipgloss.NewLayer(style.Render(text)).Draw(view, view.Bounds())
}

func metadataText(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, ansi.Strip(text))
}

type demoAction uint8

const (
	demoNone demoAction = iota
	demoNext
	demoBackground
	demoBorder
	demoQuit
)

// keyPrefix reads the demo's keys with a tmux-style prefix. Ctrl+B starts the
// prefix; then 1, 2 or 3 selects an action and a second Ctrl+B passes to the
// child. Ctrl+Q quits with or without the prefix: as in tmux, a key with no
// binding after the prefix falls through to the bindings without it. Any
// other key ends the prefix and is discarded. Only the session's capture
// handler uses it.
type keyPrefix struct{ active bool }

// prefixKey starts the demo's prefix and quitKey quits the demo.
const prefixKey, quitKey = "ctrl+b", "ctrl+q"

// route returns the action input selects and what the frame does with it.
// Releases, repeats and lone modifier or lock keys, which Kitty terminals
// report, leave the prefix as it is and pass: the frame gives a reported
// release or repeat of a consumed press its press's disposition.
func (p *keyPrefix) route(input frame.Input) (demoAction, frame.Disposition) {
	key, ok := pressedKey(input.Key)
	if !ok || isModifierKey(key.Code) {
		return demoNone, frame.Pass
	}
	if p.active {
		p.active = false
		switch {
		case key.MatchString("1"):
			return demoNext, frame.Consume
		case key.MatchString("2"):
			return demoBackground, frame.Consume
		case key.MatchString("3"):
			return demoBorder, frame.Consume
		case key.MatchString(prefixKey):
			return demoNone, frame.Pass
		case key.MatchString(quitKey):
			return demoQuit, frame.Consume
		}
		return demoNone, frame.Consume
	}
	switch {
	case key.MatchString(prefixKey):
		p.active = true
		return demoNone, frame.Consume
	case key.MatchString(quitKey):
		return demoQuit, frame.Consume
	}
	return demoNone, frame.Pass
}

// pressedKey returns the key of a press that is not a reported repeat.
func pressedKey(event uv.KeyEvent) (uv.Key, bool) {
	switch press := event.(type) {
	case uv.KeyPressEvent:
		return press.Key(), !press.IsRepeat
	case *uv.KeyPressEvent:
		if press != nil {
			return press.Key(), !press.IsRepeat
		}
	}
	return uv.Key{}, false
}

// isModifierKey reports whether code is a modifier or lock key, which a Kitty
// terminal reporting all keys sends on its own.
func isModifierKey(code rune) bool {
	switch code {
	case uv.KeyLeftShift, uv.KeyLeftAlt, uv.KeyLeftCtrl, uv.KeyLeftSuper, uv.KeyLeftHyper, uv.KeyLeftMeta,
		uv.KeyRightShift, uv.KeyRightAlt, uv.KeyRightCtrl, uv.KeyRightSuper, uv.KeyRightHyper, uv.KeyRightMeta,
		uv.KeyIsoLevel3Shift, uv.KeyIsoLevel5Shift, uv.KeyCapsLock, uv.KeyScrollLock, uv.KeyNumLock:
		return true
	}
	return false
}
