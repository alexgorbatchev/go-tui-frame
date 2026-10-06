package process

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// zombieState is SZOMB from <sys/proc.h>: the process has exited and awaits
// collection by its parent. x/sys/unix does not export it.
const zombieState = 5

// GroupExited reports whether every member of process group pgid has exited.
// kern.proc.pgrp lists members that exited but are not reaped yet, with their
// state, so a group of only such members counts as exited, as does a group
// with no members left.
func GroupExited(pgid int) (bool, error) {
	if pgid <= 0 {
		return false, fmt.Errorf("listing process group %d: invalid group ID", pgid)
	}
	rows, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return false, fmt.Errorf("listing process group %d: %w", pgid, err)
	}
	for _, row := range rows {
		if row.Proc.P_stat != zombieState {
			return false, nil
		}
	}
	return true, nil
}
