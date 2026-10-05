package frame

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

func TestWakePipeDeliveryCoalescingAndFailure(t *testing.T) {
	for _, name := range []string{"token", "full", "broken"} {
		t.Run(name, func(t *testing.T) {
			s := &session[struct{}]{}
			if err := s.openWake(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				err := s.closeWake()
				if name == "broken" {
					if !errors.Is(err, unix.EBADF) {
						t.Errorf("wake failure missing from cleanup: %v", err)
					}
				} else if err != nil {
					t.Error(err)
				}
			})
			if name == "full" {
				buf := make([]byte, 4096)
				for {
					_, err := unix.Write(s.wakeWriteFD, buf)
					if errors.Is(err, unix.EINTR) {
						continue
					}
					if errors.Is(err, unix.EAGAIN) {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if name == "broken" {
				// The owned file stays open; native Write receives a real invalid fd.
				s.wakeWriteFD = -1
			}
			s.wakeLoop()
			fds := []unix.PollFd{{Fd: int32(s.wakeReadFD), Events: unix.POLLIN}, {Fd: -1}, {Fd: -1}, {Fd: -1}}
			if n, err := unix.Poll(fds, 100); err != nil || n == 0 {
				t.Fatalf("wake did not make Poll ready: %d, %v", n, err)
			}
			if name == "broken" {
				if _, err := s.processReady(nil, fds, nil, false); !errors.Is(err, unix.EBADF) {
					t.Fatalf("wake failure missing from event loop: %v", err)
				}
				s.wakeLoop() // Further producers cannot reuse the closed descriptor.
			} else if fds[0].Revents&unix.POLLIN == 0 {
				t.Fatalf("pending token was not readable: %#v", fds[0])
			}
		})
	}
}
