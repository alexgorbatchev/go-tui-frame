package emulator

import (
	"maps"
	"slices"

	ghostty "go.mitchellh.com/libghostty"
)

// CloneState duplicates mutable storage for another observer. UV colors are
// immutable values: explicit colors are RGBA, and unchanged host-reported
// palette entries that Options.OuterColorProfile keeps as indexes are
// ansi.BasicColor or ansi.IndexedColor values whose RGB is Colors.Palette at
// that index, not the xterm default their RGBA method reports. Native
// Cell/Style/Colors are copied value snapshots.
func CloneState(state State) State {
	state.Cells = slices.Clone(state.Cells)
	state.NativeCells = slices.Clone(state.NativeCells)
	state.Modes = maps.Clone(state.Modes)
	state.ModeErrors = maps.Clone(state.ModeErrors)
	for mode, err := range state.ModeErrors {
		if native, ok := err.(*ghostty.Error); ok {
			state.ModeErrors[mode] = cloneValue(native)
		}
	}
	state.ScrollbackMaxBytes = cloneValue(state.ScrollbackMaxBytes)
	state.ScrollbackMaxLines = cloneValue(state.ScrollbackMaxLines)
	return state
}

// CloneEffect separates callback payload storage from the session's record.
func CloneEffect(effect Effect) Effect {
	effect.Bytes = slices.Clone(effect.Bytes)
	effect.Progress = cloneValue(effect.Progress)
	effect.Notification = cloneValue(effect.Notification)
	effect.SemanticPrompt = cloneValue(effect.SemanticPrompt)
	effect.Unknown = cloneValue(effect.Unknown)
	if effect.Unknown != nil {
		effect.Unknown.APC.Content = slices.Clone(effect.Unknown.APC.Content)
		effect.Unknown.OSC.Content = slices.Clone(effect.Unknown.OSC.Content)
	}
	effect.ClipboardRead = cloneValue(effect.ClipboardRead)
	if effect.ClipboardRead != nil {
		effect.ClipboardRead.MIMEs = slices.Clone(effect.ClipboardRead.MIMEs)
	}
	effect.ClipboardWrite = cloneValue(effect.ClipboardWrite)
	if effect.ClipboardWrite != nil {
		effect.ClipboardWrite.Contents = cloneContents(effect.ClipboardWrite.Contents)
	}
	return effect
}

func cloneValue[T any](value *T) *T {
	if value == nil {
		return nil
	}
	return new(*value)
}
