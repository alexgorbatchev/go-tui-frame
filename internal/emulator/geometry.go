package emulator

import (
	"errors"
	"fmt"
)

var ErrGeometry = errors.New("child changed the native grid outside the owned viewport")

// GeometryError identifies terminal-requested dimensions that cannot be
// rendered in the frame's owned viewport. The session restores its outer
// terminal on this error; DECCOLM does not implicitly resize the physical TTY.
type GeometryError struct {
	Owned  Size
	Native Size
}

func (e *GeometryError) Error() string {
	return fmt.Sprintf("%v: native %dx%d, owned %dx%d", ErrGeometry, e.Native.Cols, e.Native.Rows, e.Owned.Cols, e.Owned.Rows)
}

func (e *GeometryError) Unwrap() error { return ErrGeometry }

func (t *Terminal) checkGeometry() error {
	cols, err := t.native.Cols()
	if err != nil {
		return fmt.Errorf("reading native columns: %w", err)
	}
	rows, err := t.native.Rows()
	if err != nil {
		return fmt.Errorf("reading native rows: %w", err)
	}
	if int(cols) != t.size.Cols || int(rows) != t.size.Rows {
		return &GeometryError{Owned: t.size, Native: Size{Cols: int(cols), Rows: int(rows)}}
	}
	return nil
}
