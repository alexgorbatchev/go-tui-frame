package frame

import (
	"testing"

	"github.com/alexgorbatchev/go-tui-frame/v2/internal/input"
	uv "github.com/charmbracelet/ultraviolet"
)

func TestCaptureOnlySeesKeysAndPassesEverythingByDefault(t *testing.T) {
	var count int
	f := keyboardFilter{handler: func(Input) Disposition { count++; return Consume }}
	for _, packet := range []input.Packet{
		{Raw: []byte("paste"), Paste: true},
		{Raw: []byte("\x1b[200~"), Event: uv.PasteStartEvent{}},
		{Raw: []byte("\x1b[<0;1;1M"), Event: uv.MouseClickEvent{}},
		{Raw: []byte("unknown")},
	} {
		got, err := f.handle(packet)
		if err != nil || got != Pass {
			t.Fatalf("non-key captured: %v %v", got, err)
		}
	}
	if count != 0 {
		t.Fatal("non-key input invoked keyboard capture")
	}
	f.handler = nil
	got, err := f.handle(input.Packet{Raw: []byte("q"), Event: uv.KeyPressEvent{Code: 'q'}})
	if err != nil || got != Pass {
		t.Fatalf("default captured keyboard input: %v %v", got, err)
	}
}

func TestConsumedPressKeepsRepeatAndReleaseOutOfChild(t *testing.T) {
	var handled []uv.KeyEvent
	f := keyboardFilter{handler: func(in Input) Disposition {
		handled = append(handled, in.Key)
		if key, ok := in.Key.(uv.KeyPressEvent); ok && !key.IsRepeat && key.MatchString("f6") {
			return Consume
		}
		return Pass
	}}
	f.setReleases(true)
	for _, ev := range []uv.KeyEvent{
		uv.KeyPressEvent{Code: uv.KeyF6},
		uv.KeyPressEvent{Code: uv.KeyF6, Mod: uv.ModShift, IsRepeat: true},
		uv.KeyReleaseEvent{Code: uv.KeyF6, Mod: uv.ModShift},
	} {
		got, err := f.handle(input.Packet{Raw: []byte("event bytes"), Event: ev})
		if err != nil || got != Consume {
			t.Fatalf("consumed gesture phase leaked: %T %v %v", ev, got, err)
		}
	}
	if len(handled) != 3 {
		t.Fatalf("handler did not observe gesture phases: %d", len(handled))
	}
	got, err := f.handle(input.Packet{Raw: []byte("different press"), Event: uv.KeyPressEvent{Code: uv.KeyF6, Mod: uv.ModShift}})
	if err != nil || got != Pass {
		t.Fatalf("capture remained latched after release: %v %v", got, err)
	}
}

// A press decides who owns its release even when the two reports identify the
// key differently. The outer terminal runs the child's Kitty flags, so a child
// that turns alternate keys on or off between press and release adds or drops
// the base layout key of a non-US key such as Russian и. A win32 report
// keeps its virtual key while its code follows the modifiers. Distinct
// physical keys on a Dvorak layout keep separate gestures.
func TestConsumedPressOwnsReleaseAcrossKeyReportForms(t *testing.T) {
	for _, tt := range []struct {
		name, press, release string
		want                 Disposition
	}{
		{"base layout key dropped", "\x1b[1080::98;5u", "\x1b[1080;5:3u", Consume},
		{"base layout key added", "\x1b[1080;5u", "\x1b[1080::98;5:3u", Consume},
		{"win32 code changes", "\x1b[66;48;1080;1;8;1_", "\x1b[66;48;0;0;8;1_", Consume},
		{"different Dvorak key", "\x1b[120::98;5u", "\x1b[98::110;5:3u", Pass},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := keyboardFilter{handler: func(in Input) Disposition {
				if _, press := in.Key.(uv.KeyPressEvent); press {
					return Consume
				}
				return Pass
			}}
			f.setReleases(true)
			press, release := routerPackets(t, tt.press), routerPackets(t, tt.release)
			if len(press) != 1 || len(release) != 1 {
				t.Fatalf("decoded %d press and %d release packets", len(press), len(release))
			}
			if got, err := f.handle(press[0]); err != nil || got != Consume {
				t.Fatalf("press %q = %v, %v", tt.press, got, err)
			}
			if _, ok := release[0].Event.(uv.KeyReleaseEvent); !ok {
				t.Fatalf("release %q decoded as %T", tt.release, release[0].Event)
			}
			if got, err := f.handle(release[0]); err != nil || got != tt.want {
				t.Fatalf("release %q after press %q = %v, %v; want %v", tt.release, tt.press, got, err, tt.want)
			}
		})
	}
}

func TestLegacyCaptureDoesNotLatchWithoutReleases(t *testing.T) {
	f := keyboardFilter{handler: func(in Input) Disposition {
		if in.Key.Key().MatchString("ctrl+q") {
			return Consume
		}
		return Pass
	}}
	if got, err := f.handle(input.Packet{Event: uv.KeyPressEvent{Code: 'q', Mod: uv.ModCtrl}}); err != nil || got != Consume {
		t.Fatalf("Ctrl-Q capture = %v %v", got, err)
	}
	if got, err := f.handle(input.Packet{Event: uv.KeyPressEvent{Code: 'q', Text: "q"}}); err != nil || got != Pass {
		t.Fatalf("ordinary q was swallowed: %v %v", got, err)
	}
}

func TestCaptureCannotMutateForwardedRawBytes(t *testing.T) {
	f := keyboardFilter{handler: func(in Input) Disposition {
		in.Raw[0] = 'x'
		return Pass
	}}
	packet := input.Packet{Raw: []byte("q"), Event: uv.KeyPressEvent{Code: 'q'}}
	if _, err := f.handle(packet); err != nil {
		t.Fatal(err)
	}
	if string(packet.Raw) != "q" {
		t.Fatal("observer bytes altered delivery bytes")
	}
}

func TestForwardedPressRetainsItsRelease(t *testing.T) {
	f := keyboardFilter{handler: func(in Input) Disposition {
		if _, release := in.Key.(uv.KeyReleaseEvent); release {
			return Consume
		}
		return Pass
	}}
	f.setReleases(true)
	for _, key := range []uv.KeyEvent{uv.KeyPressEvent{Code: 'a'}, uv.KeyReleaseEvent{Code: 'a'}} {
		if got, err := f.handle(input.Packet{Event: key}); err != nil || got != Pass {
			t.Fatalf("forwarded gesture lost phase %T: %v %v", key, got, err)
		}
	}
}

func TestCaptureRejectsInvalidDisposition(t *testing.T) {
	f := keyboardFilter{handler: func(Input) Disposition { return Disposition(99) }}
	if _, err := f.handle(input.Packet{Event: uv.KeyPressEvent{Code: 'a'}}); err == nil {
		t.Fatal("invalid routing disposition accepted")
	}
}
