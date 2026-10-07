package process

import (
	"errors"
	"fmt"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// sessionScans counts walks of the host process table for session members.
var sessionScans atomic.Uint64

// SessionScans reports how many times this process has walked the host
// process table to find the members of a session. Each walk visits every host
// process, so its cost grows with the host rather than with the session.
func SessionScans() uint64 { return sessionScans.Load() }

// sessionMembers walks the host process table once and returns, in table
// order, the processes getsid reported in exactly sessionID. Processes that
// exit during the walk are skipped. Another failure ends the walk: the members
// found so far are returned with the error, which tells the caller the
// inventory is incomplete.
func sessionMembers(sessionID int) ([]int, error) {
	if sessionID <= 0 {
		return nil, fmt.Errorf("enumerating session %d: invalid session ID", sessionID)
	}
	sessionScans.Add(1)
	pids, err := processIDs()
	if err != nil {
		return nil, err
	}
	var members []int
	for _, pid := range pids {
		sid, err := unix.Getsid(pid)
		if errors.Is(err, unix.ESRCH) {
			continue
		}
		if err != nil {
			return members, fmt.Errorf("reading session for process %d: %w", pid, err)
		}
		if sid == sessionID {
			members = append(members, pid)
		}
	}
	return members, nil
}
