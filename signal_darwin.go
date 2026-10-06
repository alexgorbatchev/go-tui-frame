package frame

import "github.com/alexgorbatchev/go-tui-frame/internal/process"

// deniedByExitedGroup reports whether kill(2) refused to signal process group
// pgid with EPERM only because all its members have exited. XNU's killpg1
// (bsd/kern/kern_sig.c) skips zombie members and returns EPERM when it finds
// no member to signal, so a group whose members exited before wait reaped
// them looks like one that refuses the caller. A live member that refused
// the signal makes the EPERM a real permission failure.
func deniedByExitedGroup(pgid int) (bool, error) {
	return process.GroupExited(pgid)
}
