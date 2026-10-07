package frame

import (
	"slices"
	"time"

	"github.com/alexgorbatchev/go-tui-frame/v2/internal/process"
	"github.com/charmbracelet/x/termios"
	"golang.org/x/sys/unix"
)

// collectMetadata samples the launch leader until it is reaped, the members of
// its session, and the child PTY.
func (s *session[T]) collectMetadata() {
	if s.snapshot.Child.PID == 0 {
		return
	}
	child := &s.snapshot.Child
	child.SessionProcesses, child.SessionError = process.List(child.PID)
	if !s.waited {
		child.OperatingSystem = leaderSample(child.PID, child.SessionProcesses)
	}
	s.collectPTY()
}

// leaderSample returns the launch leader's entry in members, the inventory of
// the session it leads, so the leader is read once per sample. It reads the
// leader itself when the inventory lacks it, as after a partial enumeration
// failure or a race with the leader's exit.
func leaderSample(pid int, members []process.Snapshot) process.Snapshot {
	if i := slices.IndexFunc(members, func(p process.Snapshot) bool { return p.PID == pid }); i >= 0 {
		return members[i]
	}
	return process.Read(pid)
}

// collectPTY samples the child PTY through its master. Once cleanup has closed
// the master, the snapshot keeps the last sample.
func (s *session[T]) collectPTY() {
	if s.fd < 0 {
		return
	}
	window, windowErr := termios.GetWinsize(s.fd)
	settings, settingsErr := termios.GetTermios(s.fd)
	foreground, foregroundErr := unix.IoctlGetInt(s.fd, unix.TIOCGPGRP)
	s.snapshot.Child.PTY = PTYSnapshot{Window: window, Settings: settings, WindowError: windowErr, SettingsError: settingsErr, ObservedAt: time.Now(), ForegroundGroup: Observation[int]{Value: foreground, Available: foregroundErr == nil, Source: "TIOCGPGRP on owned PTY", Error: foregroundErr}}
}

// sampleDue reports whether a render samples processes and the PTY. A
// lifecycle render always samples, because Started and Resized carry its
// snapshot. Any other render samples only for a reader, a configured region
// or a selected event that carries a snapshot, and only when metadata changed,
// a region was invalidated, or the scroll counters changed after the last
// sample grew older than scrollSampleAge.
func (s *session[T]) sampleDue(lifecycle bool) bool {
	if lifecycle {
		return true
	}
	configured, invalidated := s.frame.regionDemand()
	if !configured && !s.events.wantsAny(snapshotEvents) {
		return false
	}
	return s.metadataDirty || invalidated ||
		s.scrollDirty && time.Since(s.snapshot.Child.PTY.ObservedAt) >= scrollSampleAge
}

// regionDemand reports whether any region is configured and whether one
// awaits drawing after an invalidation.
func (f *Frame[T]) regionDemand() (configured, invalidated bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.regions {
		configured = configured || r.draw != nil
		invalidated = invalidated || r.dirty
	}
	return configured, invalidated
}
