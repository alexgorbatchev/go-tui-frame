package process

/*
#include <unistd.h>
*/
import "C"

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

const maxObservationBytes = 16 * 1024 * 1024

func readProc(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(f, maxObservationBytes+1))
	err = errors.Join(readErr, f.Close())
	if err != nil {
		return nil, err
	}
	if len(data) > maxObservationBytes {
		return nil, fmt.Errorf("procfs observation exceeds %d bytes", maxObservationBytes)
	}
	return data, nil
}

// Read samples procfs/native syscall observations without helper programs.
func Read(pid int) Snapshot {
	if pid <= 0 {
		return missing(pid, fmt.Errorf("reading process %d: invalid PID", pid))
	}
	base := filepath.Join("/proc", strconv.Itoa(pid))
	raw, err := readProc(filepath.Join(base, "stat"))
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
	s.Args = readStrings(filepath.Join(base, "cmdline"), "kernel argument memory view; process may modify contents")
	s.Environment = readStrings(filepath.Join(base, "environ"), "kernel exec-image environment memory view; does not follow libc environment changes")
	status, err := readProc(filepath.Join(base, "status"))
	if err != nil {
		s.Status = unavailable[string]("procfs status", err)
	} else {
		s.Status = available(string(status), "procfs status; protected fields may be zeroed by kernel")
	}
	s.Resources = statResources(stat)
	raw, err = readProc(filepath.Join(base, "stat"))
	if err != nil {
		return missing(pid, err)
	}
	end, err := parseStat(string(raw))
	if err != nil {
		return missing(pid, err)
	}
	if end.start != stat.start {
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

func readStrings(path, meaning string) Field[[]string] {
	raw, err := readProc(path)
	if err != nil {
		return unavailable[[]string](path+"; "+meaning, err)
	}
	if len(raw) == 0 {
		return available([]string{}, path+"; "+meaning)
	}
	if raw[len(raw)-1] != 0 {
		return unavailable[[]string](path, fmt.Errorf("unterminated procfs string vector"))
	}
	parts := bytes.Split(raw[:len(raw)-1], []byte{0})
	values := make([]string, len(parts))
	for i, part := range parts {
		values[i] = string(part)
	}
	return available(values, path+"; "+meaning)
}

func statResources(stat procStat) Resources {
	const source = "procfs stat"
	r := Resources{
		ResidentBytes: available(stat.resident*uint64(os.Getpagesize()), source+" RSS pages converted to bytes; kernel reports approximate accounting"),
		VirtualBytes:  available(stat.virtual, source+" virtual-size bytes"),
		Threads:       available(stat.threads, source),
		Native:        available(map[string]uint64{"user_clock_ticks": stat.user, "system_clock_ticks": stat.system, "resident_pages": stat.resident, "start_clock_ticks": stat.start}, source),
	}
	hz, err := C.sysconf(C._SC_CLK_TCK)
	if hz <= 0 {
		if err == nil {
			err = ErrUnavailable
		}
		r.CPUUser = unavailable[time.Duration]("sysconf(_SC_CLK_TCK)", err)
		r.CPUSystem = unavailable[time.Duration]("sysconf(_SC_CLK_TCK)", err)
	} else {
		r.CPUUser = scaledDuration(stat.user, uint64(time.Second), uint64(hz), source+" user clock ticks")
		r.CPUSystem = scaledDuration(stat.system, uint64(time.Second), uint64(hz), source+" system clock ticks")
	}
	return r
}
