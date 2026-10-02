package input

import (
	"bytes"
	"errors"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
)

func TestFragmentedInputRetainsExactBytesAndEvent(t *testing.T) {
	tests := []struct {
		name  string
		raw   []byte
		check func(uv.Event) bool
	}{
		{"function key", []byte("\x1b[17~"), func(ev uv.Event) bool {
			key, ok := ev.(uv.KeyPressEvent)
			return ok && key.MatchString("f6")
		}},
		{"application cursor", []byte("\x1bOA"), func(ev uv.Event) bool {
			key, ok := ev.(uv.KeyPressEvent)
			return ok && key.MatchString("up")
		}},
		{"SS3 function key", []byte("\x1bOP"), func(ev uv.Event) bool {
			key, ok := ev.(uv.KeyPressEvent)
			return ok && key.MatchString("f1")
		}},
		{"C1 SS3", []byte{0x8f, 'P'}, func(ev uv.Event) bool {
			key, ok := ev.(uv.KeyPressEvent)
			return ok && key.MatchString("f1")
		}},
		{"mouse", []byte("\x1b[<0;18;5M"), func(ev uv.Event) bool {
			mouse, ok := ev.(uv.MouseClickEvent)
			return ok && mouse.Mouse().X == 17 && mouse.Mouse().Y == 4
		}},
		{"unicode", []byte("界"), func(ev uv.Event) bool {
			key, ok := ev.(uv.KeyPressEvent)
			return ok && key.Text == "界"
		}},
		{"alt unicode", []byte("\x1bé"), func(ev uv.Event) bool {
			key, ok := ev.(uv.KeyPressEvent)
			return ok && key.Code == 'é' && key.Mod == uv.ModAlt
		}},
		{"alt control", []byte("\x1b\x01"), func(ev uv.Event) bool {
			key, ok := ev.(uv.KeyPressEvent)
			return ok && key.MatchString("ctrl+alt+a")
		}},
		{"alt space", []byte("\x1b "), func(ev uv.Event) bool {
			key, ok := ev.(uv.KeyPressEvent)
			return ok && key.MatchString("alt+space")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := New()
			var packets []Packet
			for _, b := range tt.raw {
				next, err := f.Feed([]byte{b})
				if err != nil {
					t.Fatal(err)
				}
				packets = append(packets, next...)
			}
			if len(packets) != 1 || !bytes.Equal(packets[0].Raw, tt.raw) || !tt.check(packets[0].Event) {
				t.Fatalf("packets = %#v, want one complete native event for %q", packets, tt.raw)
			}
		})
	}
}

func TestOversizedCompleteControlIsRejected(t *testing.T) {
	f := New()
	raw := append([]byte("\x1b]0;"), bytes.Repeat([]byte("a"), maxSequenceBytes)...)
	raw = append(raw, '\a')
	packets, err := f.Feed(raw)
	if !errors.Is(err, ErrSequenceTooLarge) || len(packets) != 0 {
		t.Fatalf("oversized complete control accepted: %d packets, %v", len(packets), err)
	}
}

func TestManyControlParametersPassAsExactOpaquePacket(t *testing.T) {
	raw := append([]byte("\x1b["), bytes.Repeat([]byte("1;"), 64)...)
	raw = append(raw, 'u')
	packets, err := New().Feed(raw)
	if err != nil || len(packets) != 1 || !bytes.Equal(packets[0].Raw, raw) {
		t.Fatalf("control parameter framing failed: %#v %v", packets, err)
	}
}

func TestStringTerminatorCanSplitAcrossReads(t *testing.T) {
	raw := []byte("\x1b]10;rgb:aaaa/bbbb/cccc\x1b\\")
	f := New()
	var got []Packet
	for _, b := range raw {
		packets, err := f.Feed([]byte{b})
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, packets...)
	}
	if len(got) != 1 || !bytes.Equal(got[0].Raw, raw) {
		t.Fatalf("split string terminator reclassified: %#v", got)
	}
}

func TestBracketedPasteIsOpaqueAcrossFragments(t *testing.T) {
	raw := []byte("\x1b[200~hello\x11\x1b[17~\nworld\x1b[201~")
	f := New()
	var reconstructed []byte
	var inPaste bool
	var sawStart, sawEnd bool
	for _, b := range raw {
		packets, err := f.Feed([]byte{b})
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range packets {
			reconstructed = append(reconstructed, p.Raw...)
			switch p.Event.(type) {
			case uv.PasteStartEvent:
				inPaste, sawStart = true, true
			case uv.PasteEndEvent:
				inPaste, sawEnd = false, true
			case uv.KeyEvent:
				if inPaste {
					t.Fatal("paste contents became a shortcut candidate")
				}
			}
		}
	}
	if !sawStart || !sawEnd || !bytes.Equal(raw, reconstructed) {
		t.Fatalf("paste bytes changed: %q", reconstructed)
	}
}

func TestEscapeNeedsExplicitFlushAndTruncatedControlsPass(t *testing.T) {
	f := New()
	packets, err := f.Feed([]byte{0x1b})
	if err != nil || len(packets) != 0 || !f.Pending() {
		t.Fatalf("bare Escape classified before ambiguity boundary: %#v %v", packets, err)
	}
	packet, ok := f.Flush()
	key, isKey := packet.Event.(uv.KeyPressEvent)
	if !ok || !isKey || !key.MatchString("esc") || !bytes.Equal(packet.Raw, []byte{0x1b}) {
		t.Fatalf("flush = %#v, %v", packet, ok)
	}
	if _, err := f.Feed([]byte("\x1b[<0;")); err != nil {
		t.Fatal(err)
	}
	packet, ok = f.Flush()
	if !ok || packet.Event != nil || string(packet.Raw) != "\x1b[<0;" {
		t.Fatalf("truncated control reclassified or changed: %#v", packet)
	}
}

func TestAmbiguousAltIntroducersNeedDeadlineBeforeClassification(t *testing.T) {
	for _, match := range []struct{ raw, key string }{
		{"\x1b[", "alt+["}, {"\x1bO", "alt+shift+o"}, {"\x1b]", "alt+]"},
	} {
		f := New()
		packets, err := f.Feed([]byte(match.raw))
		if err != nil || len(packets) != 0 || !f.NeedsDeadline() {
			t.Fatalf("ambiguous key prematurely classified or permanently buffered: %q %#v %v", match.raw, packets, err)
		}
		packet, ok := f.Flush()
		key, isKey := packet.Event.(uv.KeyEvent)
		if !ok || !isKey || !key.Key().MatchString(match.key) || string(packet.Raw) != match.raw {
			t.Fatalf("explicit deadline lost Alt key: %#v, want %s", packet, match.key)
		}
	}
	f := New()
	if _, err := f.Feed([]byte("\x1b[17")); err != nil {
		t.Fatal(err)
	}
	if f.NeedsDeadline() {
		t.Fatal("unambiguous fragmented control acquired a key deadline")
	}
}

func TestPacketsOwnTheirBytes(t *testing.T) {
	f := New()
	raw := []byte("a")
	packets, err := f.Feed(raw)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] = 'b'
	if string(packets[0].Raw) != "a" {
		t.Fatal("packet borrowed caller input buffer")
	}
	if _, err := f.Feed([]byte("c")); err != nil {
		t.Fatal(err)
	}
	if string(packets[0].Raw) != "a" {
		t.Fatal("packet borrowed reusable framing buffer")
	}
}
