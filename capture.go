package frame

import (
	"fmt"
	"slices"

	"github.com/alexgorbatchev/go-tui-frame/internal/input"
	uv "github.com/charmbracelet/ultraviolet"
)

// keyboardFilter is owned by the session input router. When the negotiated
// protocol reports releases, the press determines who owns the final release.
type keyboardFilter struct {
	handler  func(Input) Disposition
	releases bool
	gestures map[rune]Disposition
}

func (f *keyboardFilter) setReleases(enabled bool) {
	if f.releases != enabled {
		f.gestures = nil
	}
	f.releases = enabled
}

func (f *keyboardFilter) handle(packet input.Packet) (Disposition, error) {
	key, ok := packet.Event.(uv.KeyEvent)
	if !ok || packet.Paste || f.handler == nil {
		return Pass, nil
	}
	disposition := f.handler(Input{Raw: slices.Clone(packet.Raw), Key: key})
	if disposition != Pass && disposition != Consume {
		return Pass, fmt.Errorf("capture returned invalid disposition %d", disposition)
	}
	if !f.releases {
		return disposition, nil
	}
	id := key.Key().Code
	if base := key.Key().BaseCode; base != 0 {
		id = base
	}
	previous, held := f.gestures[id]
	switch ev := key.(type) {
	case uv.KeyReleaseEvent:
		if held {
			disposition = previous
			delete(f.gestures, id)
		}
	case uv.KeyPressEvent:
		if held && ev.IsRepeat {
			if previous == Consume {
				disposition = Consume
			}
		} else {
			if f.gestures == nil {
				f.gestures = make(map[rune]Disposition)
			}
			f.gestures[id] = disposition
		}
	}
	return disposition, nil
}
