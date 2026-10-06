package frame

import (
	"errors"
	"os"
	"time"

	"github.com/alexgorbatchev/go-tui-frame/internal/emulator"
	"github.com/alexgorbatchev/go-tui-frame/internal/process"
	uv "github.com/charmbracelet/ultraviolet"
	"golang.org/x/sys/unix"
)

var (
	ErrRegionNotConfigured = errors.New("frame region is not configured")
	ErrSessionClosed       = errors.New("frame session is closed")
	ErrViewportTooSmall    = errors.New("terminal is too small for the frame")
	ErrConfigurationFrozen = errors.New("frame configuration is frozen")
	ErrAlreadyRun          = errors.New("frame can execute only one session")
	ErrObservationOverflow = errors.New("observation queue exceeded its bounded capacity")
	ErrGeometry            = emulator.ErrGeometry
)

// Size describes terminal dimensions in display cells.
type Size struct {
	Cols int
	Rows int
}

// DrawContext contains one region's drawing target and stable input values.
// View is borrowed until the callback returns. Data is copied by value;
// referenced contents must remain immutable while the frame may use them.
type DrawContext[T any] struct {
	Term Snapshot
	View uv.Screen
	Data T
}

// Snapshot is an owned observation of child and virtual-terminal state.
type Snapshot struct {
	Child      ChildSnapshot
	Terminal   TerminalSnapshot
	Viewport   Size
	Outer      Size
	ObservedAt time.Time
}

// ChildSnapshot distinguishes launch configuration from observed lifecycle.
type ChildSnapshot struct {
	PID              int
	Executable       string
	Args             []string
	Environment      []string
	Directory        string
	StartedAt        time.Time
	ProcessState     *os.ProcessState
	OperatingSystem  ProcessSnapshot
	SessionProcesses []ProcessSnapshot
	SessionError     error
	PTY              PTYSnapshot
}

// Observation distinguishes unavailable native data from an observed zero.
type Observation[T any] = process.Field[T]

// ProcessSnapshot reports current OS state, with per-field source and errors.
type ProcessSnapshot = process.Snapshot

// ProcessResources contains native memory, CPU, and thread observations.
type ProcessResources = process.Resources

// GeometryError identifies a terminal-requested grid outside the viewport.
type GeometryError = emulator.GeometryError

// PTYSnapshot records the child's actual window, line discipline, and group.
// Reads are sequential; they do not establish a history of settings changes.
type PTYSnapshot struct {
	Window                     *unix.Winsize
	Settings                   *unix.Termios
	ForegroundGroup            Observation[int]
	WindowError, SettingsError error
	ObservedAt                 time.Time
}

// TerminalSnapshot owns its cell storage rather than borrowing emulator cells.
// On a 256-color outer terminal, a cell color that uses an inherited palette
// entry the child has not redefined is an ansi.BasicColor (entries 0-15) or
// ansi.IndexedColor index; a 16-color outer terminal does this for entries
// 0-15 only. Its RGB is Native.Colors.Palette at that index; the index's RGBA
// method reports the xterm default instead. Other cell colors, including every
// color on true-color and colorless outer terminals, are resolved RGB values.
type TerminalSnapshot struct {
	Title     string
	Directory string
	Size      Size
	Cells     []uv.Cell
	Cursor    Cursor
	Alternate bool
	Native    NativeState
}

// NativeState owns the complete terminal observations exposed by libghostty.
// ModeErrors preserve unavailable getters rather than substituting false.
type NativeState = emulator.State

// ProtocolEffect owns an ordered native terminal request or generated reply.
type ProtocolEffect = emulator.Effect

// Cursor describes the child-local cursor position and visibility.
type Cursor struct {
	X       int
	Y       int
	Visible bool
}

// Disposition selects host capture or ordinary child input routing.
type Disposition uint8

const (
	Pass Disposition = iota
	Consume
)

// Input supplements original transport bytes with a native keyboard event.
// Capture receives recognized keyboard events; paste and unknown controls
// continue through ordinary routing. Raw is an owned observation copy.
type Input struct {
	Raw []byte
	Key uv.KeyEvent
}

// EventKind identifies an observation's transport or lifecycle boundary.
type EventKind string

const (
	OuterInput  EventKind = "outer-input"
	ChildOutput EventKind = "child-output"
	ChildInput  EventKind = "child-input"
	// Started carries the child's first snapshot, once the session has entered
	// the outer terminal and painted its first frame.
	Started EventKind = "started"
	// Exited follows Started exactly once, before Run returns, when Wait reports
	// the launch leader: after the child exits, after cancellation, or after a
	// session error that ends the session while the child runs. Its snapshot's
	// Child.ProcessState is the state Result.ProcessState reports. A session
	// that fails before Started emits neither. Exited is lost only when
	// observation fails: after a callback panic or runtime.Goexit, or when it
	// overflows the queue, which Run's error then reports as
	// ErrObservationOverflow.
	Exited       EventKind = "exited"
	Resized      EventKind = "resized"
	Captured     EventKind = "captured"
	StateChanged EventKind = "state-changed"
	Protocol     EventKind = "protocol"
	Routed       EventKind = "routed"
)

// Event contains durable copies and stream offsets for an observed operation.
type Event struct {
	Kind        EventKind
	Sequence    uint64
	Offset      uint64
	Time        time.Time
	Bytes       []byte
	Input       *Input
	Snapshot    *Snapshot
	Disposition Disposition
	Origin      string
	Error       error
	Effect      *ProtocolEffect
}

// Result separates the child's exit status from drain and restoration errors.
type Result struct {
	ProcessState *os.ProcessState
	DrainError   error
	CleanupError error
}
