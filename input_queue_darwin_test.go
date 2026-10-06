package frame

import (
	"io"
	"testing"

	"golang.org/x/sys/unix"
)

// A terminal revoked after input pauses, before the session starts watching it
// for a hangup, ends the session as a disconnect. kqueue refuses a filter on
// the revoked terminal; an error there would skip the child's graceful
// termination.
func TestPausedOuterInputReportsRevokedTerminal(t *testing.T) {
	s, outer, _ := newQueueSession(t, gestureModes, io.Discard)
	fillChild(t, s, inputQueueLimit-10)
	gestures, _ := heldGestures()
	if _, err := outer.Write(gestures); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, readBufferSize)
	if paused, _ := step(t, s, buf); !paused {
		t.Fatal("outer reads did not pause while the child reads nothing")
	}
	if err := unix.Revoke(s.console.input.Name()); err != nil {
		t.Fatal(err)
	}
	if _, disconnected := step(t, s, buf); !disconnected {
		t.Fatal("revoked terminal was not reported as disconnected")
	}
}
