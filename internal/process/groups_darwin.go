package process

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Process state and flag from <sys/proc.h>, which x/sys/unix does not export.
const (
	// zombieState is SZOMB: the process has exited and awaits collection by
	// its parent.
	zombieState = 5
	// exitingFlag is P_WEXIT, which kern.proc reports once the process has
	// started to exit. An exit cannot be undone, and the flag stays set.
	exitingFlag = 0x00002000
)

// GroupExited reports whether every member of process group pgid has exited
// or is exiting. kern.proc.pgrp lists members until their parent reaps them:
// one whose exit has begun carries P_WEXIT, and one that finished is SZOMB.
// A group with no members left counts as exited too.
func GroupExited(pgid int) (bool, error) {
	if pgid <= 0 {
		return false, fmt.Errorf("listing process group %d: invalid group ID", pgid)
	}
	rows, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return false, fmt.Errorf("listing process group %d: %w", pgid, err)
	}
	for _, row := range rows {
		if row.Proc.P_stat != zombieState && row.Proc.P_flag&exitingFlag == 0 {
			return false, nil
		}
	}
	return true, nil
}
