package frame

import (
	"errors"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alexgorbatchev/go-tui-frame/v2/internal/process"
	ghostty "go.mitchellh.com/libghostty"
	"golang.org/x/sys/unix"
)

func TestSnapshotSeparatesNativeProcessAndPTYStorage(t *testing.T) {
	s := Snapshot{Child: ChildSnapshot{OperatingSystem: process.Read(os.Getpid()), PTY: PTYSnapshot{Window: &unix.Winsize{Col: 80}, Settings: &unix.Termios{}}}}
	c := cloneSnapshot(s)
	if len(c.Child.OperatingSystem.Args.Value) == 0 {
		t.Fatal("native process args unavailable")
	}
	c.Child.OperatingSystem.Args.Value[0] = "consumer mutation"
	if s.Child.OperatingSystem.Args.Value[0] == "consumer mutation" {
		t.Fatal("native process storage is borrowed")
	}
	c.Child.PTY.Window.Col = 1
	c.Child.PTY.Settings.Lflag = unix.ICANON
	if s.Child.PTY.Window.Col != 80 || s.Child.PTY.Settings.Lflag != 0 {
		t.Fatal("PTY snapshot storage is borrowed")
	}
}

func TestObservationBudgetIncludesProtocolNamesAndCommands(t *testing.T) {
	for _, tt := range []struct {
		name   string
		effect ProtocolEffect
	}{
		{"semantic command", ProtocolEffect{SemanticPrompt: &ghostty.TerminalSemanticPrompt{Command: strings.Repeat("x", observationByteLimit)}}},
		{"semantic error", ProtocolEffect{SemanticPrompt: &ghostty.TerminalSemanticPrompt{Error: strings.Repeat("x", observationByteLimit)}}},
		{"clipboard read name", ProtocolEffect{ClipboardRead: &ghostty.ClipboardRead{Name: strings.Repeat("x", observationByteLimit)}}},
		{"clipboard write name", ProtocolEffect{ClipboardWrite: &ghostty.ClipboardWrite{Name: strings.Repeat("x", observationByteLimit)}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// An idle dispatcher admits any lone record, so weigh the effect
			// against the budget left beside an executing callback.
			d, entered, _ := pausedDispatcher(t, func(Event) {})
			if err := d.emit(Event{Kind: Started}); err != nil {
				t.Fatal(err)
			}
			<-entered
			if err := d.emit(Event{Kind: Protocol, Effect: &tt.effect}); !errors.Is(err, ErrObservationOverflow) {
				t.Fatalf("large protocol metadata admitted: %v", err)
			}
		})
	}
}

// startSessionLeader starts a sleeping child that leads its own session, as
// the frame starts the launch leader, and kills and reaps it when t ends.
func startSessionLeader(t *testing.T) int {
	t.Helper()
	cmd := childCommand(t)
	cmd.Env = append(cmd.Env, "FRAME_TEST_MODE=sleep")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cmd.Process.Kill(); err != nil {
			t.Error(err)
		}
		_ = cmd.Wait() // The kill above ends the sleeper, so Wait reports that signal.
	})
	return cmd.Process.Pid
}

// A sample reads the launch leader once: its OperatingSystem sample is the
// leader's entry in the session inventory. Read stamps ObservedAt when it
// starts, so a second read of the leader would carry a later time.
func TestCollectMetadataReadsTheLeaderOnce(t *testing.T) {
	pid := startSessionLeader(t)
	s := &session[struct{}]{fd: -1, snapshot: Snapshot{Child: ChildSnapshot{PID: pid}}}
	s.collectMetadata()
	child := s.snapshot.Child
	if child.SessionError != nil {
		t.Fatal(child.SessionError)
	}
	i := slices.IndexFunc(child.SessionProcesses, func(p process.Snapshot) bool { return p.PID == pid })
	if i < 0 {
		t.Fatalf("leader %d missing from session inventory %#v", pid, child.SessionProcesses)
	}
	if leader := child.SessionProcesses[i]; !child.OperatingSystem.ObservedAt.Equal(leader.ObservedAt) {
		t.Fatalf("leader read twice: OperatingSystem observed at %v, inventory entry at %v", child.OperatingSystem.ObservedAt, leader.ObservedAt)
	}
	if p := child.OperatingSystem; p.PID != pid || !p.SessionID.Available || p.SessionID.Value != pid {
		t.Fatalf("leader sample: %#v", p)
	}
}

// The leader's inventory entry serves as its sample. An inventory that lacks
// the leader, as after a partial enumeration failure or a race with its exit,
// makes the sample a fresh read of the leader.
func TestLeaderSampleFallsBackToRead(t *testing.T) {
	pid := startSessionLeader(t)
	members, err := process.List(pid)
	if err != nil {
		t.Fatal(err)
	}
	listed := time.Now()
	tests := []struct {
		name    string
		members []process.Snapshot
		fresh   bool
	}{
		{"leader listed", members, false},
		{"leader missing", []process.Snapshot{process.Read(os.Getpid())}, true},
		{"empty inventory", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := leaderSample(pid, tt.members)
			if got.PID != pid || !got.SessionID.Available || got.SessionID.Value != pid {
				t.Fatalf("leader sample: %#v", got)
			}
			if fresh := !got.ObservedAt.Before(listed); fresh != tt.fresh {
				t.Fatalf("sample observed at %v, inventory read before %v: fresh = %v, want %v", got.ObservedAt, listed, fresh, tt.fresh)
			}
		})
	}
}
