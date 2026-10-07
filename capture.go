package frame

import (
	"fmt"
	"slices"

	"github.com/alexgorbatchev/go-tui-frame/internal/input"
	uv "github.com/charmbracelet/ultraviolet"
)

// keyboardFilter is owned by the session input router. When the negotiated
// protocol reports releases, the press determines who owns its reported
// repeats and its release. A repeat the terminal sends as plain text or a
// legacy byte decodes as a new press, which takes over the held key. A release
// reports the key rather than its text, so it matches a plain-text press only
// when the text is the key's unshifted character or, for an ASCII letter, its
// capital. Otherwise, as for Shift+1 typing ! or Shift+й typing Й, the
// handler's answer routes that release.
type keyboardFilter struct {
	handler  func(Input) Disposition
	releases bool
	gestures []gesture
}

// gesture records who owns a held key's repeats and release.
type gesture struct {
	code, base  rune
	disposition Disposition
}

// continues reports whether k is a later report of the held key. Two reports
// that both carry a base layout key compare that physical key, because a
// win32 report keeps its virtual key while its code follows the modifiers.
// Otherwise they compare the code, which a Kitty report carries with or
// without alternate keys: the outer terminal runs the child's Kitty flags, so
// a child flag change can add or drop the base layout key between a press and
// its release.
func (g gesture) continues(k uv.Key) bool {
	if g.base != 0 && k.BaseCode != 0 {
		return g.base == k.BaseCode
	}
	return g.code == k.Code
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
	k := key.Key()
	held := -1
	for i, g := range f.gestures {
		if g.continues(k) {
			held = i
			break
		}
	}
	switch ev := key.(type) {
	case uv.KeyReleaseEvent:
		if held >= 0 {
			disposition = f.gestures[held].disposition
			f.gestures = slices.Delete(f.gestures, held, held+1)
		}
	case uv.KeyPressEvent:
		switch {
		case held >= 0 && ev.IsRepeat:
			disposition = f.gestures[held].disposition
		case held >= 0:
			f.gestures[held] = gesture{code: k.Code, base: k.BaseCode, disposition: disposition}
		default:
			f.gestures = append(f.gestures, gesture{code: k.Code, base: k.BaseCode, disposition: disposition})
		}
	}
	return disposition, nil
}
