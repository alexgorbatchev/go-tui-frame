package process

import (
	"errors"
	"fmt"
	"slices"

	"golang.org/x/sys/unix"
)

// Groups returns the process groups currently belonging to exactly sessionID,
// in ascending order. Enumeration races process exits; disappearing PIDs are
// ignored. Any other failure ends the enumeration and returns the groups found
// so far with the error, so a caller can still act on them without treating
// the inventory as complete.
func Groups(sessionID int) ([]int, error) {
	members, err := sessionMembers(sessionID)
	groups := make(map[int]struct{})
	for _, pid := range members {
		pgid, ok, groupErr := memberGroup(sessionID, pid)
		if groupErr != nil {
			err = errors.Join(err, groupErr)
			break
		}
		if ok {
			groups[pgid] = struct{}{}
		}
	}
	result := make([]int, 0, len(groups))
	for pgid := range groups {
		result = append(result, pgid)
	}
	slices.Sort(result)
	return result, err
}

// memberGroup reads the process group of pid, a member getsid placed in
// sessionID. It reports false when pid has exited or left the session since.
func memberGroup(sessionID, pid int) (int, bool, error) {
	pgid, err := unix.Getpgid(pid)
	if errors.Is(err, unix.ESRCH) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("reading group for process %d: %w", pid, err)
	}
	// Recheck ownership after the second syscall. A process can change
	// sessions between reads, and PIDs can be recycled after an exit.
	sid, err := unix.Getsid(pid)
	if errors.Is(err, unix.ESRCH) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("rechecking session for process %d: %w", pid, err)
	}
	return pgid, sid == sessionID && pgid > 0, nil
}
