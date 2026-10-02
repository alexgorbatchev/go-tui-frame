package process

import (
	"errors"
	"fmt"
	"slices"

	"golang.org/x/sys/unix"
)

// Groups returns the process groups currently belonging to exactly sessionID.
// Enumeration races process exits; disappearing PIDs are ignored, other errors
// are returned so cancellation cannot silently claim a complete inventory.
func Groups(sessionID int) ([]int, error) {
	if sessionID <= 0 {
		return nil, fmt.Errorf("enumerating session %d: invalid session ID", sessionID)
	}
	pids, err := processIDs()
	if err != nil {
		return nil, err
	}
	groups := make(map[int]struct{})
	for _, pid := range pids {
		sid, err := unix.Getsid(pid)
		if errors.Is(err, unix.ESRCH) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading session for process %d: %w", pid, err)
		}
		if sid != sessionID {
			continue
		}
		pgid, err := unix.Getpgid(pid)
		if errors.Is(err, unix.ESRCH) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading group for process %d: %w", pid, err)
		}
		// Recheck ownership after the second syscall. A process can change
		// sessions between reads, and PIDs can be recycled after an exit.
		sid, err = unix.Getsid(pid)
		if errors.Is(err, unix.ESRCH) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("rechecking session for process %d: %w", pid, err)
		}
		if sid == sessionID && pgid > 0 {
			groups[pgid] = struct{}{}
		}
	}
	result := make([]int, 0, len(groups))
	for pgid := range groups {
		result = append(result, pgid)
	}
	slices.Sort(result)
	return result, nil
}
