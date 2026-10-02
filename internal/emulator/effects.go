package emulator

import (
	"errors"
	"fmt"
	"slices"

	ghostty "go.mitchellh.com/libghostty"
)

type EffectKind string

const (
	Reply            EffectKind = "reply"
	Bell             EffectKind = "bell"
	TitleChanged     EffectKind = "title"
	DirectoryChanged EffectKind = "directory"
	Progress         EffectKind = "progress"
	Notification     EffectKind = "notification"
	Unknown          EffectKind = "unknown"
	RenderHold       EffectKind = "render-hold"
	SemanticPrompt   EffectKind = "semantic-prompt"
	Reset            EffectKind = "reset"
	ClipboardRead    EffectKind = "clipboard-read"
	ClipboardWrite   EffectKind = "clipboard-write"
)

// Effect owns every byte slice and native value copied during a callback.
// Only the field selected by Kind is meaningful. Reply bytes are drained in
// callback order for the session's single PTY writer.
type Effect struct {
	Kind           EffectKind
	Bytes          []byte
	Text           string
	Held           bool
	Progress       *ghostty.TerminalProgressReport
	Notification   *ghostty.TerminalDesktopNotification
	Unknown        *ghostty.TerminalUnknownSequence
	SemanticPrompt *ghostty.TerminalSemanticPrompt
	ClipboardRead  *ghostty.ClipboardRead
	ClipboardWrite *ghostty.ClipboardWrite
}

func (t *Terminal) Effects() []Effect {
	if t == nil {
		return nil
	}
	effects := t.effects
	t.effects = nil
	return effects
}

func (t *Terminal) nativeOptions(opts Options) []ghostty.TerminalOption {
	var attributes ghostty.DeviceAttributes
	hasAttributes := opts.DeviceAttributes != nil
	if hasAttributes {
		attributes = *opts.DeviceAttributes
	}
	return []ghostty.TerminalOption{
		ghostty.WithWritePty(func(_ *ghostty.Terminal, data []byte) {
			t.effects = append(t.effects, Effect{Kind: Reply, Bytes: slices.Clone(data)})
		}),
		ghostty.WithBell(func(_ *ghostty.Terminal) { t.effects = append(t.effects, Effect{Kind: Bell}) }),
		ghostty.WithTitleChanged(func(native *ghostty.Terminal) { t.textEffect(TitleChanged, native.Title) }),
		ghostty.WithPwdChanged(func(native *ghostty.Terminal) { t.textEffect(DirectoryChanged, native.Pwd) }),
		ghostty.WithProgressReport(func(_ *ghostty.Terminal, report ghostty.TerminalProgressReport) {
			t.effects = append(t.effects, Effect{Kind: Progress, Progress: &report})
		}),
		ghostty.WithDesktopNotification(func(_ *ghostty.Terminal, notification ghostty.TerminalDesktopNotification) {
			t.effects = append(t.effects, Effect{Kind: Notification, Notification: &notification})
		}),
		ghostty.WithUnknownSequence(func(_ *ghostty.Terminal, sequence ghostty.TerminalUnknownSequence) {
			sequence.APC.Content = slices.Clone(sequence.APC.Content)
			sequence.OSC.Content = slices.Clone(sequence.OSC.Content)
			t.effects = append(t.effects, Effect{Kind: Unknown, Unknown: &sequence})
		}),
		ghostty.WithRenderHold(func(_ *ghostty.Terminal, held bool) {
			if held {
				// The callback precedes remaining bytes in this VTWrite, making
				// this the only safe point to capture the last complete frame.
				if err := t.captureVisual(); err != nil {
					t.callbackErr = errors.Join(t.callbackErr, err)
				}
			}
			t.held = held
			t.effects = append(t.effects, Effect{Kind: RenderHold, Held: held})
		}),
		ghostty.WithSemanticPrompt(func(_ *ghostty.Terminal, event ghostty.TerminalSemanticPrompt) {
			t.effects = append(t.effects, Effect{Kind: SemanticPrompt, SemanticPrompt: &event})
		}),
		ghostty.WithReset(func(_ *ghostty.Terminal) { t.effects = append(t.effects, Effect{Kind: Reset}) }),
		ghostty.WithXtversion(func(_ *ghostty.Terminal) string { return opts.Version }),
		ghostty.WithSizeReport(func(_ *ghostty.Terminal) (ghostty.SizeReportSize, bool) {
			return ghostty.SizeReportSize{Rows: uint16(t.size.Rows), Columns: uint16(t.size.Cols), CellWidth: t.size.CellWidthPx, CellHeight: t.size.CellHeightPx}, true
		}),
		ghostty.WithDeviceAttributes(func(_ *ghostty.Terminal) (ghostty.DeviceAttributes, bool) { return attributes, hasAttributes }),
		ghostty.WithClipboardRead(func(_ *ghostty.Terminal, request ghostty.ClipboardRead) ghostty.ClipboardReadReply {
			observed := request
			observed.MIMEs = slices.Clone(request.MIMEs)
			t.effects = append(t.effects, Effect{Kind: ClipboardRead, ClipboardRead: &observed})
			if opts.ClipboardRead != nil {
				request.MIMEs = slices.Clone(request.MIMEs)
				return opts.ClipboardRead(request)
			}
			return ghostty.ClipboardReadReply{Result: ghostty.ClipboardReadUnsupported}
		}),
		ghostty.WithClipboardWrite(func(_ *ghostty.Terminal, request ghostty.ClipboardWrite) ghostty.ClipboardWriteReply {
			observed := request
			observed.Contents = cloneContents(request.Contents)
			t.effects = append(t.effects, Effect{Kind: ClipboardWrite, ClipboardWrite: &observed})
			if opts.ClipboardWrite != nil {
				request.Contents = cloneContents(request.Contents)
				return opts.ClipboardWrite(request)
			}
			return ghostty.ClipboardWriteReply{Result: ghostty.ClipboardWriteUnsupported}
		}),
	}
}

func cloneContents(contents []ghostty.ClipboardContent) []ghostty.ClipboardContent {
	contents = slices.Clone(contents)
	for i := range contents {
		contents[i].Data = slices.Clone(contents[i].Data)
	}
	return contents
}

func (t *Terminal) textEffect(kind EffectKind, read func() (string, error)) {
	text, err := read()
	if err != nil {
		t.callbackErr = errors.Join(t.callbackErr, fmt.Errorf("reading %s effect: %w", kind, err))
		return
	}
	t.effects = append(t.effects, Effect{Kind: kind, Text: text})
}
