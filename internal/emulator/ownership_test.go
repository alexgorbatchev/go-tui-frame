package emulator

import (
	"testing"

	ghostty "go.mitchellh.com/libghostty"
)

func TestCloneStateSeparatesStorage(t *testing.T) {
	em, err := New(Options{Size: Size{Cols: 8, Rows: 2}, ScrollbackMaxBytes: new(uint(65536)), ScrollbackMaxLines: new(uint(20))})
	if err != nil {
		t.Fatal(err)
	}
	defer em.Close()
	writeTerminal(t, em, "x\x1b[?2004h")
	original := terminalState(t, em)
	cloned := CloneState(original)
	cloned.Cells[0].Content = "changed"
	cloned.NativeCells[0] = NativeCell{}
	cloned.Modes[ghostty.ModeBracketedPaste] = false
	cloned.ModeErrors[ghostty.ModeBracketedPaste] = ghostty.ErrInvalidValue
	*cloned.ScrollbackMaxLines = 1
	*cloned.ScrollbackMaxBytes = 1
	if original.Cells[0].Content != "x" || !original.Modes[ghostty.ModeBracketedPaste] || len(original.ModeErrors) != 0 || *original.ScrollbackMaxLines != 20 || *original.ScrollbackMaxBytes != 65536 {
		t.Fatal("clone mutated original state")
	}
	hasText, err := original.NativeCells[0].Raw.HasText()
	if err != nil || !hasText {
		t.Fatalf("original raw cell changed: %v, %v", hasText, err)
	}
}

func TestCloneEffectSeparatesObservedPayloads(t *testing.T) {
	em := newTerminal(t, 8, 2)
	writeTerminal(t, em, "\x1b[6n\x1b]52;c;aGVsbG8=\x1b\\\x1b]9999;unknown\x07")
	for _, original := range em.Effects() {
		cloned := CloneEffect(original)
		switch cloned.Kind {
		case Reply:
			cloned.Bytes[0] = 'x'
			if original.Bytes[0] != '\x1b' {
				t.Fatal("reply bytes alias")
			}
		case ClipboardWrite:
			cloned.ClipboardWrite.Contents[0].Data[0] = 'x'
			cloned.ClipboardWrite.Name = "changed"
			if string(original.ClipboardWrite.Contents[0].Data) != "hello" || original.ClipboardWrite.Name == "changed" {
				t.Fatal("clipboard aliases")
			}
		case Unknown:
			cloned.Unknown.OSC.Content[0] = 'x'
			if string(original.Unknown.OSC.Content) != "9999;unknown" {
				t.Fatal("unknown payload aliases")
			}
		}
	}
}

func TestClipboardHandlersCannotMutateObservations(t *testing.T) {
	em, err := New(Options{
		Size: Size{Cols: 8, Rows: 2},
		ClipboardWrite: func(request ghostty.ClipboardWrite) ghostty.ClipboardWriteReply {
			request.Contents[0].Data[0] = 'x'
			return ghostty.ClipboardWriteReply{Result: ghostty.ClipboardWriteSuccess}
		},
		ClipboardRead: func(request ghostty.ClipboardRead) ghostty.ClipboardReadReply {
			if len(request.MIMEs) > 0 {
				request.MIMEs[0] = "changed"
			}
			return ghostty.ClipboardReadReply{Result: ghostty.ClipboardReadSuccess, Contents: []ghostty.ClipboardContent{{MIME: "text/plain", Data: []byte("provided")}}}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer em.Close()
	writeTerminal(t, em, "\x1b]52;c;aGVsbG8=\x1b\\\x1b]52;c;?\x1b\\")
	var reply string
	for _, effect := range em.Effects() {
		switch effect.Kind {
		case ClipboardWrite:
			if string(effect.ClipboardWrite.Contents[0].Data) != "hello" {
				t.Fatal("handler mutated observed clipboard data")
			}
		case ClipboardRead:
			for _, mime := range effect.ClipboardRead.MIMEs {
				if mime == "changed" {
					t.Fatal("handler mutated observed MIME data")
				}
			}
		case Reply:
			reply += string(effect.Bytes)
		}
	}
	if reply != "\x1b]52;c;cHJvdmlkZWQ=\x1b\\" {
		t.Fatalf("clipboard response = %q", reply)
	}
}

// Ghostty keeps one active tracking mode: setting 9, 1000, 1002 or 1003 makes
// it active and resetting any of them turns tracking off. MouseTracking keeps
// the native getter's value, which is set while any of the four bits is.
func TestMouseTrackingModeObservationUsesNativeState(t *testing.T) {
	for _, tt := range []struct {
		name, controls string
		want           ghostty.MouseTrackingMode
		anyBit         bool
	}{
		{"default", "", ghostty.MouseTrackingNone, false},
		{"X10", "\x1b[?9h", ghostty.MouseTrackingX10, true},
		{"normal", "\x1b[?1000h", ghostty.MouseTrackingNormal, true},
		{"button", "\x1b[?1002h", ghostty.MouseTrackingButton, true},
		{"any", "\x1b[?1003h", ghostty.MouseTrackingAny, true},
		{"reset after upgrade", "\x1b[?1000h\x1b[?1002h\x1b[?1002l", ghostty.MouseTrackingNone, true},
		{"set and reset", "\x1b[?1000h\x1b[?1000l", ghostty.MouseTrackingNone, false},
		{"later mode replaces earlier", "\x1b[?1003h\x1b[?1000h", ghostty.MouseTrackingNormal, true},
		{"reset of an inactive mode", "\x1b[?1003h\x1b[?1000h\x1b[?1003l", ghostty.MouseTrackingNone, true},
		{"pixel format without geometry", "\x1b[?1016h\x1b[?1002h", ghostty.MouseTrackingButton, true},
		{"restored mode", "\x1b[?1002s\x1b[?1000h\x1b[?1002h\x1b[?1002r", ghostty.MouseTrackingNone, true},
		{"full reset", "\x1b[?1003h\x1bc", ghostty.MouseTrackingNone, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			em := newTerminal(t, 8, 2)
			writeTerminal(t, em, tt.controls)
			// A full reset emits its own effects; only the observation must not.
			em.Effects()
			s := terminalState(t, em)
			if s.MouseTrackingMode != tt.want || s.MouseTracking != tt.anyBit {
				t.Fatalf("mouse tracking mode = %v (any bit %v), want %v (any bit %v)", s.MouseTrackingMode, s.MouseTracking, tt.want, tt.anyBit)
			}
			if len(em.Effects()) != 0 {
				t.Fatal("state observation generated PTY effects")
			}
		})
	}
}

func TestModifyOtherKeysObservationUsesNativeState(t *testing.T) {
	em := newTerminal(t, 8, 2)
	for _, tt := range []struct {
		name      string
		fragments []string
		want      bool
	}{
		{"default", nil, false},
		{"fragmented enable", []string{"\x1b[>4;", "2m"}, true},
		{"with Kitty flags", []string{"\x1b[>31u"}, true},
		{"unknown SGR", []string{"\x1b[4;0m"}, true},
		{"reset format", []string{"\x1b[>4;0m"}, false},
		{"mode one is not two", []string{"\x1b[>4;1m"}, false},
		{"reenable", []string{"\x1b[>4;2m"}, true},
		{"malformed extra parameters", []string{"\x1b[>4;0;9m"}, true},
		{"default resource reset", []string{"\x1b[>m"}, false},
		{"enable before RIS", []string{"\x1b[>4;2m"}, true},
		{"full reset", []string{"\x1bc"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, fragment := range tt.fragments {
				writeTerminal(t, em, fragment)
			}
			s := terminalState(t, em)
			if s.ModifyOtherKeys2 != tt.want {
				t.Fatalf("modifyOtherKeys2 = %v, want %v", s.ModifyOtherKeys2, tt.want)
			}
			if len(em.Effects()) != 0 && tt.name != "full reset" {
				t.Fatal("state observation generated PTY effects")
			}
		})
	}
}
