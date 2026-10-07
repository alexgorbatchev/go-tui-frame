package process

/*
#include <unistd.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const maxObservationBytes = 16 * 1024 * 1024

// procReader reuses one scratch buffer across the procfs files that a single
// Read observes. Every caller copies what it keeps, so each result aliases the
// buffer and stays valid only until the next read.
type procReader struct {
	buf []byte
}

// procReadChunk is the smallest step by which the scratch buffer grows.
const procReadChunk = 4096

func (r *procReader) read(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, readErr := r.fill(f)
	if err := errors.Join(readErr, f.Close()); err != nil {
		return nil, err
	}
	if len(data) > maxObservationBytes {
		return nil, fmt.Errorf("procfs observation exceeds %d bytes", maxObservationBytes)
	}
	return data, nil
}

// fill reads f to EOF into the scratch buffer, stopping one byte past
// maxObservationBytes so read can report an oversized observation.
func (r *procReader) fill(f *os.File) ([]byte, error) {
	const limit = maxObservationBytes + 1
	data := r.buf[:0]
	for len(data) < limit {
		if len(data) == cap(data) {
			data = slices.Grow(data, procReadChunk)
		}
		n, err := f.Read(data[len(data):min(cap(data), limit)])
		data = data[:len(data)+n]
		if err != nil {
			r.buf = data
			if err == io.EOF {
				return data, nil
			}
			return nil, err
		}
	}
	r.buf = data
	return data, nil
}

// Read samples procfs/native syscall observations without helper programs.
func Read(pid int) Snapshot {
	if pid <= 0 {
		return missing(pid, fmt.Errorf("reading process %d: invalid PID", pid))
	}
	var r procReader
	base := filepath.Join("/proc", strconv.Itoa(pid))
	raw, err := r.read(filepath.Join(base, "stat"))
	if err != nil {
		return missing(pid, err)
	}
	stat, err := parseStat(string(raw))
	if err != nil {
		return missing(pid, err)
	}
	const source = "procfs stat"
	s := missing(pid, ErrUnavailable)
	s.ParentPID = available(stat.parent, source)
	s.Name = available(stat.name, source+" comm (native truncated name)")
	s.State = available(stat.state, source+" state code")
	s.StartIdentity = available(strconv.FormatUint(stat.start, 10), source+" start clock ticks since boot")
	if stat.foreground > 0 {
		s.ForegroundGroup = available(stat.foreground, source+" controlling-terminal foreground group")
	}
	sid, err := unix.Getsid(pid)
	if err != nil {
		s.SessionID = unavailable[int]("getsid", err)
	} else {
		s.SessionID = available(sid, "getsid")
	}
	pgid, err := unix.Getpgid(pid)
	if err != nil {
		s.ProcessGroup = unavailable[int]("getpgid", err)
	} else {
		s.ProcessGroup = available(pgid, "getpgid")
	}
	s.Directory = readLink(filepath.Join(base, "cwd"))
	s.Executable = readLink(filepath.Join(base, "exe"))
	s.Args = r.readStrings(filepath.Join(base, "cmdline"), "kernel argument memory view; process may modify contents")
	s.Environment = r.readStrings(filepath.Join(base, "environ"), "kernel exec-image environment memory view; does not follow libc environment changes")
	status, err := r.read(filepath.Join(base, "status"))
	if err != nil {
		s.Status = unavailable[string]("procfs status", err)
	} else {
		s.Status = available(string(status), "procfs status; protected fields may be zeroed by kernel")
	}
	s.Resources = statResources(stat)
	raw, err = r.read(filepath.Join(base, "stat"))
	if err != nil {
		return missing(pid, err)
	}
	start, err := parseStatStart(raw)
	if err != nil {
		return missing(pid, err)
	}
	if start != stat.start {
		return missing(pid, ErrIdentityChanged)
	}
	return s
}

func readLink(path string) Field[string] {
	value, err := os.Readlink(path)
	if err != nil {
		return unavailable[string](path, err)
	}
	return available(value, path)
}

func (r *procReader) readStrings(path, meaning string) Field[[]string] {
	source := path + "; " + meaning
	raw, err := r.read(path)
	if err != nil {
		return unavailable[[]string](source, err)
	}
	if len(raw) == 0 {
		return available([]string{}, source)
	}
	if raw[len(raw)-1] != 0 {
		return unavailable[[]string](path, fmt.Errorf("unterminated procfs string vector"))
	}
	// One conversion and one split keep the allocation count independent of
	// the element count; the elements share one immutable backing string.
	return available(strings.Split(string(raw[:len(raw)-1]), "\x00"), source)
}

func statResources(stat procStat) Resources {
	const source = "procfs stat"
	r := Resources{
		ResidentBytes: available(stat.resident*uint64(os.Getpagesize()), source+" RSS pages converted to bytes; kernel reports approximate accounting"),
		VirtualBytes:  available(stat.virtual, source+" virtual-size bytes"),
		Threads:       available(stat.threads, source),
		Native:        available(map[string]uint64{"user_clock_ticks": stat.user, "system_clock_ticks": stat.system, "resident_pages": stat.resident, "start_clock_ticks": stat.start}, source),
	}
	if hz, err := clockTicks(); err != nil {
		const tickSource = "sysconf(_SC_CLK_TCK)"
		r.CPUUser = unavailable[time.Duration](tickSource, err)
		r.CPUSystem = unavailable[time.Duration](tickSource, err)
	} else {
		r.CPUUser = scaledDuration(stat.user, uint64(time.Second), hz, source+" user clock ticks converted to nanoseconds")
		r.CPUSystem = scaledDuration(stat.system, uint64(time.Second), hz, source+" system clock ticks converted to nanoseconds")
	}
	return r
}

// clockTicks is the procfs clock-tick rate. It is fixed for the life of the
// process, so one sysconf call serves every Read.
var clockTicks = sync.OnceValues(func() (uint64, error) {
	hz, err := C.sysconf(C._SC_CLK_TCK)
	if hz <= 0 {
		if err == nil {
			err = ErrUnavailable
		}
		return 0, err
	}
	return uint64(hz), nil
})
