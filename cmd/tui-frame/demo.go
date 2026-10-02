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
	keyHints        = "Ctrl+1 layout | Ctrl+2 colour | Ctrl+3 border | Ctrl+Q quit"
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
		badge := lipgloss.NewLayer(badgeStyle.Render("Ctrl+1 layout")).Z(1)
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
	text := metadata + "\n" + keyHints
	if ctx.Data.Agent {
		paint(ctx.View, lipgloss.NewStyle(), text)
		return
	}
	bg := lipgloss.Color([...]string{slate, navy, teal}[ctx.Data.Demo])
	style := lipgloss.NewStyle().Background(bg).Foreground(lipgloss.Color(white))
	switch ctx.Data.Demo {
	case 0:
		paint(ctx.View, style, text)
	case 1:
		paint(ctx.View, style, "")
		lineStyle := style.Width(ctx.View.Bounds().Dx()).MaxWidth(ctx.View.Bounds().Dx()).MaxHeight(1)
		metadataLayer := lipgloss.NewLayer(lineStyle.Render(metadata))
		controlsLayer := lipgloss.NewLayer(lineStyle.Bold(true).Render(keyHints)).Y(1)
		lipgloss.NewCompositor(metadataLayer, controlsLayer).Draw(ctx.View, ctx.View.Bounds())
	case 2:
		style = style.Border(lipgloss.NormalBorder(), false, false, false, true).
			BorderForeground(lipgloss.Color(white)).BorderBackground(bg)
		paint(ctx.View, style, text)
	}
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
	demoPass demoAction = iota
	demoNext
	demoBackground
	demoBorder
	demoQuit
	demoRelease
)

func actionFor(input frame.Input) demoAction {
	if input.Key == nil {
		return demoPass
	}
	key := input.Key.Key()
	action := demoPass
	switch {
	case key.MatchString("ctrl+1"):
		action = demoNext
	case key.MatchString("ctrl+2"):
		action = demoBackground
	case key.MatchString("ctrl+3"):
		action = demoBorder
	case key.MatchString("ctrl+q"):
		action = demoQuit
	}
	if action != demoPass {
		switch input.Key.(type) {
		case uv.KeyReleaseEvent, *uv.KeyReleaseEvent:
			return demoRelease
		}
	}
	return action
}
