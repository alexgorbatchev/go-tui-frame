package process

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestGroupExitedFollowsMemberStates(t *testing.T) {
	tests := []struct {
		name   string
		script string
		exited bool // wait until the leader has exited
		reap   bool
		want   bool
	}{
		{name: "live leader", script: "sleep 60", want: false},
		{name: "exited leader with live member", script: "sleep 60 & exit 0", exited: true, want: false},
		{name: "unreaped exited members", script: "exit 0", exited: true, want: true},
		{name: "reaped members", script: "exit 0", exited: true, reap: true, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pgid, reap := startGroup(t, tt.script)
			if tt.exited {
				awaitZombie(t, pgid)
			}
			if tt.reap {
				reap()
			}
			got, err := GroupExited(pgid)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("GroupExited(%d) = %v, want %v", pgid, got, tt.want)
			}
		})
	}
}

func TestGroupExitedRejectsInvalidGroup(t *testing.T) {
	for _, pgid := range []int{0, -1} {
		if _, err := GroupExited(pgid); err == nil {
			t.Errorf("GroupExited(%d) accepted an invalid group ID", pgid)
		}
	}
}

// startGroup runs script under /bin/sh as the leader of a new process group
// and returns a function that reaps the leader. Cleanup kills the group's
// live members and reaps the leader unless the test did; it never signals a
// reaped leader's ID, which may already name another group.
func startGroup(t *testing.T, script string) (int, func()) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	reaped := false
	reap := func() {
		reaped = true
		var exit *exec.ExitError
		if err := cmd.Wait(); err != nil && !errors.As(err, &exit) {
			t.Error(err)
		}
	}
	t.Cleanup(func() {
		if reaped {
			return
		}
		// Darwin reports EPERM when only unreaped exited members remain.
		if err := unix.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, unix.EPERM) {
			t.Error(err)
		}
		reap()
	})
	return pgid, reap
}

// awaitZombie waits until pid has exited and awaits reaping by its parent.
func awaitZombie(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		row, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
		if err != nil {
			t.Fatal(err)
		}
		if row.Proc.P_stat == zombieState {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d did not exit", pid)
}
