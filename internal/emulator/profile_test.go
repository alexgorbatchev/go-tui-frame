package emulator

import (
	"bytes"
	"testing"
)

func TestUnrenderedGraphicsDoesNotAdvertiseSupport(t *testing.T) {
	em := newTerminal(t, 12, 3)
	query := []byte("\x1b_Ga=q,i=42,s=1,v=1,f=24;AAAA\x1b\\")
	for _, prefix := range []string{"", "\x1bc", "\x1b[?1049h"} {
		if _, err := em.Write(append([]byte(prefix), query...)); err != nil {
			t.Fatal(err)
		}
		for _, effect := range em.Effects() {
			if effect.Kind == Reply && bytes.Contains(effect.Bytes, []byte("OK")) {
				t.Fatalf("unrendered graphics received successful native capability reply %q", effect.Bytes)
			}
		}
	}
}
