package frame

import (
	"time"

	"github.com/alexgorbatchev/go-tui-frame/internal/process"
	"github.com/charmbracelet/x/termios"
	"golang.org/x/sys/unix"
)

func (s *session[T]) collectMetadata() {
	if s.snapshot.Child.PID == 0 {
		return
	}
	child := &s.snapshot.Child
	if !s.waited {
		child.OperatingSystem = process.Read(child.PID)
	}
	child.SessionProcesses, child.SessionError = process.List(child.PID)
	window, windowErr := termios.GetWinsize(s.fd)
	settings, settingsErr := termios.GetTermios(s.fd)
	foreground, foregroundErr := unix.IoctlGetInt(s.fd, unix.TIOCGPGRP)
	child.PTY = PTYSnapshot{Window: window, Settings: settings, WindowError: windowErr, SettingsError: settingsErr, ObservedAt: time.Now(), ForegroundGroup: Observation[int]{Value: foreground, Available: foregroundErr == nil, Source: "TIOCGPGRP on owned PTY", Error: foregroundErr}}
}

func (f *Frame[T]) regionsDirty() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.regions {
		if r.dirty {
			return true
		}
	}
	return false
}
