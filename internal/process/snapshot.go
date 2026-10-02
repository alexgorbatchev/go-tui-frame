package process

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"slices"
	"time"

	"golang.org/x/sys/unix"
)

// Field distinguishes an unavailable observation from an observed zero value.
// Source describes the native view, which may differ from application internals.
type Field[T any] struct {
	Value     T
	Available bool
	Source    string
	Error     error
}

func scaledDuration(ticks, numer, denom uint64, source string) Field[time.Duration] {
	if denom == 0 {
		return unavailable[time.Duration](source, fmt.Errorf("native CPU clock has zero denominator"))
	}
	hi, lo := bits.Mul64(ticks, numer)
	if hi >= denom {
		return unavailable[time.Duration](source, fmt.Errorf("native CPU time overflows duration"))
	}
	nanos, remainder := bits.Div64(hi, lo, denom)
	_ = remainder // Sub-nanosecond fractions do not fit time.Duration.
	if nanos > math.MaxInt64 {
		return unavailable[time.Duration](source, fmt.Errorf("native CPU time overflows duration"))
	}
	return available(time.Duration(nanos), source+" converted to nanoseconds")
}

// Resources contains native process accounting, not an atomic resource sample.
type Resources struct {
	ResidentBytes Field[uint64]
	VirtualBytes  Field[uint64]
	CPUUser       Field[time.Duration]
	CPUSystem     Field[time.Duration]
	Threads       Field[int]
	Native        Field[map[string]uint64]
}

// Snapshot contains current OS observations, separate from launch configuration.
// Args and Environment are kernel-exposed memory views, not current application
// objects. Field reads are sequential; StartIdentity guards detected PID reuse.
type Snapshot struct {
	PID             int
	ObservedAt      time.Time
	ParentPID       Field[int]
	SessionID       Field[int]
	ProcessGroup    Field[int]
	ForegroundGroup Field[int]
	Name            Field[string]
	State           Field[string]
	Directory       Field[string]
	Executable      Field[string]
	Args            Field[[]string]
	Environment     Field[[]string]
	StartIdentity   Field[string]
	Status          Field[string]
	Resources       Resources
}

var ErrUnavailable = errors.New("native observation unavailable")
var ErrIdentityChanged = errors.New("process identity changed while sampling")

func available[T any](value T, source string) Field[T] {
	return Field[T]{Value: value, Available: true, Source: source}
}

func unavailable[T any](source string, err error) Field[T] {
	return Field[T]{Source: source, Error: err}
}

func missing(pid int, err error) Snapshot {
	return Snapshot{
		PID: pid, ObservedAt: time.Now(),
		ParentPID:       unavailable[int]("native process identity", err),
		SessionID:       unavailable[int]("getsid", err),
		ProcessGroup:    unavailable[int]("getpgid", err),
		ForegroundGroup: unavailable[int]("native controlling terminal", err),
		Name:            unavailable[string]("native process identity", err),
		State:           unavailable[string]("native process state", err),
		Directory:       unavailable[string]("native current directory", err),
		Executable:      unavailable[string]("native executable path", err),
		Args:            unavailable[[]string]("native argument view", err),
		Environment:     unavailable[[]string]("native environment view", err),
		StartIdentity:   unavailable[string]("native process start identity", err),
		Status:          unavailable[string]("native process status", err),
		Resources: Resources{
			ResidentBytes: unavailable[uint64]("native resource accounting", err),
			VirtualBytes:  unavailable[uint64]("native resource accounting", err),
			CPUUser:       unavailable[time.Duration]("native resource accounting", err),
			CPUSystem:     unavailable[time.Duration]("native resource accounting", err),
			Threads:       unavailable[int]("native resource accounting", err),
			Native:        unavailable[map[string]uint64]("native resource accounting", err),
		},
	}
}

// List returns currently observable members of exactly sessionID, ordered by PID.
// A member that exits during sampling remains visible with per-field errors.
func List(sessionID int) ([]Snapshot, error) {
	if sessionID <= 0 {
		return nil, fmt.Errorf("enumerating session %d: invalid session ID", sessionID)
	}
	pids, err := processIDs()
	if err != nil {
		return nil, err
	}
	var result []Snapshot
	for _, pid := range pids {
		sid, err := unix.Getsid(pid)
		if errors.Is(err, unix.ESRCH) {
			continue
		}
		if err != nil {
			return result, fmt.Errorf("reading session for process %d: %w", pid, err)
		}
		if sid != sessionID {
			continue
		}
		s := Read(pid)
		if s.SessionID.Available && s.SessionID.Value != sessionID {
			continue
		}
		result = append(result, s)
	}
	slices.SortFunc(result, func(a, b Snapshot) int { return a.PID - b.PID })
	return result, nil
}

// Clone separates all mutable observation storage.
func Clone(s Snapshot) Snapshot {
	s.Args.Value = slices.Clone(s.Args.Value)
	s.Environment.Value = slices.Clone(s.Environment.Value)
	if s.Resources.Native.Value != nil {
		values := make(map[string]uint64, len(s.Resources.Native.Value))
		for key, value := range s.Resources.Native.Value {
			values[key] = value
		}
		s.Resources.Native.Value = values
	}
	return s
}
