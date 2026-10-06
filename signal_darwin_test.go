package frame

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Process state and flag from <sys/proc.h>, which x/sys/unix does not export.
const (
	zombieState = 5          // SZOMB: exited, awaiting collection by its parent
	exitingFlag = 0x00002000 // P_WEXIT: working on exiting
)

const (
	// lingerSeconds bounds how long the exiting child's socket close waits for
	// its peer. Closing the peer ends the wait sooner.
	lingerSeconds = 30
	// saturationTimeout is how long a full send buffer must stay full before
	// the child treats the connection as unable to take more data.
	saturationTimeout = 100 * time.Millisecond
)

// A group whose members are exiting or have exited is gone before wait reaps
// them, but Darwin's kill(2) reports EPERM for it. XNU's killpg1 finds no
// member to signal: pgrp_iterate skips a member once its exit has begun and
// proc_find refuses it, and killpg1 skips the zombie it later becomes. A child
// that dies from the session's SIGTERM before its SIGCONT reaches either state.
func TestSignalChildTreatsExitedGroupAsGone(t *testing.T) {
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
	})
	deadline := time.Now().Add(20 * time.Second)
	var child *lingeringChild
	for attempt := 1; ; attempt++ {
		if time.Now().After(deadline) {
			t.Fatalf("no group was signalled while its member was exiting in %d attempts", attempt-1)
		}
		c := startLingeringChild(t, listener, deadline)
		// The signal counts only if the member was exiting before and after it.
		if c.awaitExiting(t, deadline) {
			err := signalChild(c.pgid, syscall.SIGCONT)
			if c.exiting(t) {
				if err != nil {
					t.Fatalf("signal group of an exiting member: %v", err)
				}
				child = c
				break
			}
		}
		t.Logf("attempt %d: the child finished exiting before its group was signalled; retrying", attempt)
		c.finish(t)
	}

	child.release(t)
	awaitZombie(t, child.pgid)
	if err := unix.Kill(-child.pgid, 0); !errors.Is(err, unix.EPERM) {
		t.Fatalf("probe of zombie group = %v, want EPERM", err)
	}
	if err := signalChild(child.pgid, syscall.SIGCONT); err != nil {
		t.Fatalf("signal group of an unreaped exited member: %v", err)
	}

	child.reap(t)
	awaitGone(t, -child.pgid, "reaped group still exists")
	if err := signalChild(child.pgid, syscall.SIGCONT); err != nil {
		t.Fatalf("signal reaped group: %v", err)
	}
}

// lingeringChild is a TestLingeringExitChild process leading its own group,
// with the test's end of its TCP connection.
type lingeringChild struct {
	cmd    *exec.Cmd
	pgid   int
	peer   net.Conn
	reaped bool
}

func startLingeringChild(t *testing.T, listener *net.TCPListener, deadline time.Time) *lingeringChild {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLingeringExitChild$")
	cmd.Env = append(os.Environ(), "FRAME_TEST_LINGER_ADDR="+listener.Addr().String())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &lingeringChild{cmd: cmd, pgid: cmd.Process.Pid}
	t.Cleanup(func() { c.finish(t) })
	if err := listener.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	peer, err := listener.Accept()
	if err != nil {
		t.Fatalf("accept child connection: %v", err)
	}
	c.peer = peer
	return c
}

// awaitExiting waits until the child's exit has begun and kill(2) refuses its
// group. It reports false if the child became a zombie first.
func (c *lingeringChild) awaitExiting(t *testing.T, deadline time.Time) bool {
	t.Helper()
	for time.Now().Before(deadline) {
		state, exiting := processState(t, c.pgid)
		if state == zombieState {
			return false
		}
		if exiting && errors.Is(unix.Kill(-c.pgid, 0), unix.EPERM) {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("child %d did not start exiting", c.pgid)
	return false
}

// exiting reports whether the child's exit has begun and not yet finished.
func (c *lingeringChild) exiting(t *testing.T) bool {
	t.Helper()
	state, exiting := processState(t, c.pgid)
	return exiting && state != zombieState
}

// release closes the test's end of the connection, which resets it and ends
// the child's lingering close.
func (c *lingeringChild) release(t *testing.T) {
	t.Helper()
	if c.peer == nil {
		return
	}
	if err := c.peer.Close(); err != nil {
		t.Error(err)
	}
	c.peer = nil
}

// reap waits for the child once. A reaped leader's ID may name another group.
func (c *lingeringChild) reap(t *testing.T) {
	t.Helper()
	if c.reaped {
		return
	}
	c.reaped = true
	if err := c.cmd.Wait(); err != nil {
		t.Error(err)
	}
}

func (c *lingeringChild) finish(t *testing.T) {
	t.Helper()
	c.release(t)
	c.reap(t)
}

// processState reads pid's kern.proc.pid state and whether its exit has begun.
func processState(t *testing.T, pid int) (int8, bool) {
	t.Helper()
	row, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		t.Fatal(err)
	}
	return row.Proc.P_stat, row.Proc.P_flag&exitingFlag != 0
}

// awaitZombie waits until pid has exited and awaits reaping by its parent.
func awaitZombie(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if state, _ := processState(t, pid); state == zombieState {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("process %d did not become a zombie", pid)
}

// TestLingeringExitChild runs as startLingeringChild's child. It fills a TCP
// connection to the test, which never reads, and sets SO_LINGER_SEC on the
// blocking socket. Exit closes the socket only after proc_find stops
// returning the process, and the close waits for the unsent data, so the
// child stays between the start of its exit and the zombie state until the
// test closes its end.
func TestLingeringExitChild(t *testing.T) {
	addr := os.Getenv("FRAME_TEST_LINGER_ADDR")
	if addr == "" {
		return
	}
	os.Exit(fillLingeringSocket(addr))
}

// fillLingeringSocket returns the child's exit status: 0 once the socket is
// full and lingers, otherwise the step that failed.
func fillLingeringSocket(addr string) int {
	peer, err := netip.ParseAddrPort(addr)
	if err != nil {
		return 90
	}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		return 91
	}
	if err := unix.Connect(fd, &unix.SockaddrInet4{Port: int(peer.Port()), Addr: peer.Addr().As4()}); err != nil {
		return 92
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		return 93
	}
	buf := make([]byte, 64<<10)
	for {
		_, err := unix.Write(fd, buf)
		if err == nil || errors.Is(err, unix.EINTR) {
			continue
		}
		if !errors.Is(err, unix.EAGAIN) {
			return 94
		}
		// Data still moving into the peer's receive buffer frees send room.
		// The connection is full once no room frees up for a while.
		ready, err := unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}, int(saturationTimeout.Milliseconds()))
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return 95
		}
		if ready == 0 {
			break
		}
	}
	// XNU's soclose does not linger on a non-blocking socket.
	if err := unix.SetNonblock(fd, false); err != nil {
		return 96
	}
	if err := unix.SetsockoptLinger(fd, unix.SOL_SOCKET, unix.SO_LINGER_SEC, &unix.Linger{Onoff: 1, Linger: lingerSeconds}); err != nil {
		return 97
	}
	return 0
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
