package emulator

import (
	"errors"
	"testing"

	ghostty "go.mitchellh.com/libghostty"
)

func TestNativeKeyEncodingFollowsChildModes(t *testing.T) {
	em := newTerminal(t, 12, 3)
	event, err := ghostty.NewKeyEvent()
	if err != nil {
		t.Fatal(err)
	}
	defer event.Close()
	event.SetAction(ghostty.KeyActionPress)
	event.SetKey(ghostty.KeyArrowUp)
	for _, tt := range []struct{ name, output, want string }{
		{"normal cursor", "", "\x1b[A"},
		{"application cursor", "\x1b[?1h", "\x1bOA"},
		{"restored cursor", "\x1b[?1l", "\x1b[A"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			writeTerminal(t, em, tt.output)
			got, err := em.EncodeKey(event)
			if err != nil || string(got) != tt.want {
				t.Fatalf("EncodeKey = %q, %v, want %q", got, err, tt.want)
			}
		})
	}
	writeTerminal(t, em, "\x1b[>3u")
	event.SetKey(ghostty.KeyQ)
	event.SetUTF8("q")
	event.SetUnshiftedCodepoint('q')
	event.SetMods(ghostty.ModCtrl)
	event.SetAction(ghostty.KeyActionRelease)
	got, err := em.EncodeKey(event)
	if err != nil || string(got) != "\x1b[113;5:3u" {
		t.Fatalf("Kitty release = %q, %v", got, err)
	}
	writeTerminal(t, em, "\x1b[<u\x1b[>4;2m")
	event.SetAction(ghostty.KeyActionPress)
	got, err = em.EncodeKey(event)
	if err != nil || string(got) != "\x1b[27;5;113~" {
		t.Fatalf("modifyOtherKeys = %q, %v", got, err)
	}
}

func TestNativeMouseEncodingUsesTrackingAndGeometry(t *testing.T) {
	em := newTerminal(t, 12, 3)
	event, err := ghostty.NewMouseEvent()
	if err != nil {
		t.Fatal(err)
	}
	defer event.Close()
	event.SetButton(ghostty.MouseButtonLeft)
	event.SetAction(ghostty.MouseActionPress)
	event.SetPosition(ghostty.MousePosition{X: 2, Y: 1})
	writeTerminal(t, em, "\x1b[?1002;1006h")
	got, err := em.EncodeMouse(event, false)
	if err != nil || string(got) != "\x1b[<0;3;2M" {
		t.Fatalf("unit-cell press = %q, %v", got, err)
	}
	event.SetAction(ghostty.MouseActionMotion)
	got, err = em.EncodeMouse(event, true)
	if err != nil || string(got) != "\x1b[<32;3;2M" {
		t.Fatalf("drag = %q, %v", got, err)
	}
	event.ClearButton()
	got, err = em.EncodeMouse(event, false)
	if err != nil || len(got) != 0 {
		t.Fatalf("unrequested motion = %q, %v", got, err)
	}
	writeTerminal(t, em, "\x1b[?1016h")
	if _, err := em.EncodeMouse(event, false); !errors.Is(err, ErrPixelGeometry) {
		t.Fatalf("unmeasured pixel encoding = %v", err)
	}
	if err := em.Resize(Size{Cols: 12, Rows: 3, CellWidthPx: 9, CellHeightPx: 18}); err != nil {
		t.Fatal(err)
	}
	event.SetButton(ghostty.MouseButtonLeft)
	event.SetAction(ghostty.MouseActionPress)
	event.SetPosition(ghostty.MousePosition{X: 20, Y: 30})
	got, err = em.EncodeMouse(event, false)
	if err != nil || string(got) != "\x1b[<0;20;30M" {
		t.Fatalf("pixel press = %q, %v", got, err)
	}
	event.SetButton(ghostty.MouseButtonTen)
	if _, err := em.EncodeMouse(event, false); !errors.Is(err, ErrMouseButton) {
		t.Fatalf("unsupported native button = %v", err)
	}
}
