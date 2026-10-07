package frame

import (
	"time"

	"github.com/alexgorbatchev/go-tui-frame/v2/internal/emulator"
	ghostty "go.mitchellh.com/libghostty"
)

// Coalesce fragmented child output within one display frame without polling
// metadata or redrawing an idle session.
const (
	renderInterval         = time.Second / 60
	synchronizedOutputMode = 2026
	// scrollSampleAge bounds how old the process and PTY sample a scroll-only
	// region redraw reuses. A child that streams lines scrolls on every
	// rendered frame, and each sample scans every process on the host.
	scrollSampleAge = 250 * time.Millisecond
)

// Regions follow terminal metadata and application invalidations. Cell damage,
// cursor movement and memory accounting do not invalidate cached region canvases.
// A metadata change also resamples the child's processes and PTY.
func regionMetadataChanged(a, b emulator.State) bool {
	return a.Title != b.Title || a.Directory != b.Directory || a.Size != b.Size ||
		a.Alternate != b.Alternate || regionModesChanged(a.Modes, b.Modes) ||
		a.KittyKeyboardFlags != b.KittyKeyboardFlags || a.ModifyOtherKeys2 != b.ModifyOtherKeys2 ||
		a.MouseTracking != b.MouseTracking || a.MouseTrackingMode != b.MouseTrackingMode || a.MouseShape != b.MouseShape ||
		!equalLimit(a.ScrollbackMaxBytes, b.ScrollbackMaxBytes) || !equalLimit(a.ScrollbackMaxLines, b.ScrollbackMaxLines)
}

// scrollbackChanged reports a change of the scroll counters, which redraws
// regions but resamples processes and the PTY only after scrollSampleAge.
// Ghostty counts every line that scrolls off the primary screen and subtracts
// a page when it prunes, so these counters change on every frame that scrolls.
func scrollbackChanged(a, b emulator.State) bool {
	return a.Scrollbar != b.Scrollbar || a.ScrollbackRows != b.ScrollbackRows || a.TotalRows != b.TotalRows
}

func regionModesChanged(a, b map[ghostty.Mode]bool) bool {
	if len(a) != len(b) {
		return true
	}
	for mode, value := range a {
		switch mode {
		case ghostty.ModeCursorVisible, ghostty.ModeCursorBlinking, ghostty.ModeSyncOutput:
			// Presentation changes update the child display, not frame chrome.
			continue
		}
		if other, ok := b[mode]; !ok || other != value {
			return true
		}
	}
	return false
}

func equalLimit(a, b *uint) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
