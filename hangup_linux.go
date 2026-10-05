package frame

import "golang.org/x/sys/unix"

// hangupWatch needs no kernel object on Linux. Poll reports POLLHUP and
// POLLERR whatever events requests (do_pollfd in fs/select.c), and a closed
// pty master wakes its slave's pollers, so the paused outer terminal stays in
// the poll set with no events.
type hangupWatch struct{}

func (*hangupWatch) watch(int, bool) error { return nil }

func (*hangupWatch) pollFD() unix.PollFd { return unix.PollFd{Fd: -1} }

func (*hangupWatch) hungUp() (bool, error) { return false, nil }

func (*hangupWatch) close() error { return nil }
