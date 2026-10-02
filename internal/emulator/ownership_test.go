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
