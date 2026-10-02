package emulator

import (
	"errors"
	"testing"
)

func TestChildRequestedColumnResizeFailsExplicitly(t *testing.T) {
	em := newTerminal(t, 12, 3)
	n, err := em.Write([]byte("\x1b[?40h\x1b[?3h"))
	if n == 0 || !errors.Is(err, ErrGeometry) {
		t.Fatalf("DECCOLM Write = %d, %v", n, err)
	}
	var changed *GeometryError
	if !errors.As(err, &changed) || changed.Owned.Cols != 12 || changed.Native.Cols != 132 || changed.Native.Rows != 3 {
		t.Fatalf("geometry observation = %#v", changed)
	}
	if _, err := em.State(); !errors.Is(err, ErrGeometry) {
		t.Fatalf("corrupt geometry State = %v", err)
	}
}

func TestResizeDoesNotReturnPreviousWriteCallbackError(t *testing.T) {
	em := newTerminal(t, 12, 3)
	if _, err := em.Write([]byte("\x1b[?40h\x1b[?3h\x1b[?2026h")); !errors.Is(err, ErrGeometry) {
		t.Fatalf("native column/hold failure: %v", err)
	}
	if err := em.ReleaseHold(); err != nil {
		t.Fatal(err)
	}
	if err := em.Resize(Size{Cols: 12, Rows: 3}); err != nil {
		t.Fatalf("successful resize retained previous callback error: %v", err)
	}
	if _, err := em.State(); err != nil {
		t.Fatal(err)
	}
}
