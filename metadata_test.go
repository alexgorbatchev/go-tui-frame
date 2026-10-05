package frame

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/alexgorbatchev/go-tui-frame/internal/process"
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
