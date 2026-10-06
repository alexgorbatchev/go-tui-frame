package frame

import (
	"errors"
	"os"
	"os/exec"
	"slices"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A group whose members have all exited is gone before wait reaps them.
// Darwin's kill(2) reports EPERM for it: XNU's killpg1 skips zombie members
// and finds none to signal. A child that dies from the session's SIGTERM
// before its SIGCONT reaches this state.
func TestSignalChildTreatsUnreapedGroupAsGone(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			if err := cmd.Wait(); err != nil {
				t.Error(err)
			}
		}
	})
	awaitRefusedGroup(t, pgid)
	if err := signalChild(pgid, syscall.SIGCONT); err != nil {
		t.Fatalf("signal group of unreaped exited members: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	reaped = true
	if err := unix.Kill(-pgid, 0); !errors.Is(err, unix.ESRCH) {
		t.Fatalf("probe of reaped group = %v, want ESRCH", err)
	}
	if err := signalChild(pgid, syscall.SIGCONT); err != nil {
		t.Fatalf("signal reaped group: %v", err)
	}
}

// awaitRefusedGroup waits until kill(2) refuses process group pgid. For a
// group of the caller's own unreaped children, that means all have exited.
func awaitRefusedGroup(t *testing.T, pgid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		err := unix.Kill(-pgid, 0)
		if errors.Is(err, unix.EPERM) {
			return
		}
		if err != nil {
			t.Fatalf("probe group %d: %v", pgid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("group %d still accepts signals", pgid)
}

// Darwin's kill(2) reports the same EPERM for a group with a live member that
// refuses the caller. That is a permission failure, and signalChild reports it.
func TestSignalChildReportsGroupThatRefusesSignals(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root may signal every process group except launchd's, so no group refuses this caller")
	}
	pgid, ok := refusingGroup(t)
	if !ok {
		t.Skip("no live root-owned process group refuses this caller's signals")
	}
	// Signal 0 checks permission and delivers nothing.
	if err := signalChild(pgid, 0); !errors.Is(err, unix.EPERM) {
		t.Fatalf("signal live group %d that refuses this caller = %v, want EPERM", pgid, err)
	}
}

// refusingGroup finds a live root-owned process group leader whose group
// refuses signals from this caller.
func refusingGroup(t *testing.T) (int, bool) {
	t.Helper()
	rows, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		t.Fatal(err)
	}
	var leaders []int
	for _, row := range rows {
		pid := int(row.Proc.P_pid)
		// Group 1 is launchd's, and kill(2) reads -1 as a broadcast.
		if pid > 1 && int(row.Eproc.Pgid) == pid && row.Eproc.Ucred.Uid == 0 {
			leaders = append(leaders, pid)
		}
	}
	// Daemons that launchd starts early run until shutdown.
	slices.Sort(leaders)
	for _, pid := range leaders {
		// kill(2) refuses a live process the caller may not signal and
		// accepts a zombie, so EPERM proves the leader is alive.
		if !errors.Is(unix.Kill(pid, 0), unix.EPERM) {
			continue
		}
		if errors.Is(unix.Kill(-pid, 0), unix.EPERM) {
			return pid, true
		}
	}
	return 0, false
}
