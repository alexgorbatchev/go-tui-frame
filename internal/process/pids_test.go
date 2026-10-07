package process

import (
	"os"
	"slices"
	"testing"
)

// processIDs lists every live host process, including this test and a fresh
// child session, and never a non-positive PID.
func TestProcessIDsListsLiveHostProcesses(t *testing.T) {
	leader, member := processFamily(t)
	pids, err := processIDs()
	if err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{os.Getpid(), leader, member} {
		if !slices.Contains(pids, pid) {
			t.Fatalf("process IDs omit live process %d", pid)
		}
	}
	seen := make(map[int]struct{}, len(pids))
	for _, pid := range pids {
		if pid <= 0 {
			t.Fatalf("process IDs include non-positive PID %d", pid)
		}
		if _, ok := seen[pid]; ok {
			t.Fatalf("process IDs list PID %d twice", pid)
		}
		seen[pid] = struct{}{}
	}
}
