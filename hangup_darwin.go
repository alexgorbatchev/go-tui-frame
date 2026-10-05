package frame

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// hangupWatch reports an outer-terminal hangup while outer input is paused.
// Darwin's poll registers no kernel filter for a descriptor polled for no
// events, and for POLLHUP alone it registers a one-shot read filter that
// unread input consumes without reporting. A kqueue read filter with EV_CLEAR
// stays registered: it reports the input already waiting once, then only new
// terminal activity, and the hangup as EV_EOF. Its kqueue descriptor joins the
// session's poll set while input is paused.
type hangupWatch struct {
	kq             int
	open, watching bool
}

// watch registers fd while on is true and removes the registration otherwise.
// A terminal revoked before the watch starts is reported as hung up.
func (w *hangupWatch) watch(fd int, on bool) (bool, error) {
	if on == w.watching {
		return false, nil
	}
	if !w.open {
		kq, err := unix.Kqueue()
		if err != nil {
			return false, fmt.Errorf("create terminal hangup queue: %w", err)
		}
		unix.CloseOnExec(kq)
		w.kq, w.open = kq, true
	}
	flags := unix.EV_ADD | unix.EV_CLEAR
	if !on {
		flags = unix.EV_DELETE
	}
	change := make([]unix.Kevent_t, 1)
	unix.SetKevent(&change[0], fd, unix.EVFILT_READ, flags)
	for {
		_, err := unix.Kevent(w.kq, change, nil, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.ENOENT) {
			// kqueue refuses a filter on a revoked terminal, and removes the
			// one-shot filter of a terminal revoked while watched.
			w.watching = false
			return on, nil
		}
		if err != nil {
			return false, fmt.Errorf("watch terminal hangup: %w", err)
		}
		w.watching = on
		return false, nil
	}
}

// pollFD returns the kqueue descriptor to poll while watching.
func (w *hangupWatch) pollFD() unix.PollFd {
	if !w.watching {
		return unix.PollFd{Fd: -1}
	}
	return unix.PollFd{Fd: int32(w.kq), Events: unix.POLLIN}
}

// hungUp drains the event that made the kqueue readable and reports whether
// the terminal hung up. Any other event only means input arrived meanwhile.
func (w *hangupWatch) hungUp() (bool, error) {
	events := make([]unix.Kevent_t, 1)
	n, err := unix.Kevent(w.kq, nil, events, &unix.Timespec{})
	if errors.Is(err, unix.EINTR) {
		return false, nil // The queue stays readable; the next poll retries.
	}
	if err != nil {
		return false, fmt.Errorf("read terminal hangup queue: %w", err)
	}
	return n > 0 && events[0].Flags&unix.EV_EOF != 0, nil
}

func (w *hangupWatch) close() error {
	if !w.open {
		return nil
	}
	w.open, w.watching = false, false
	if err := unix.Close(w.kq); err != nil {
		return fmt.Errorf("close terminal hangup queue: %w", err)
	}
	return nil
}
