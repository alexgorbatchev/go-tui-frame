package frame

import "github.com/alexgorbatchev/go-tui-frame/v2/internal/process"

// deniedByExitedGroup reports whether kill(2) refused to signal process group
// pgid with EPERM only because all its members have exited or are exiting.
// XNU's killpg1 (bsd/kern/kern_sig.c) returns EPERM when it finds no member
// to signal, and it finds none of those: pgrp_iterate (bsd/kern/kern_proc.c)
// skips a member once its exit has begun and proc_find refuses it, and
// killpg1 skips the zombie it later becomes. A live member that refused the
// signal makes the EPERM a real permission failure.
func deniedByExitedGroup(pgid int) (bool, error) {
	return process.GroupExited(pgid)
}
