package process

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// direntBufferSize matches the getdents buffer the os package reads
// directories with.
const direntBufferSize = 8192

// Offsets of the linux_dirent64 fields that getdents64 fills in. Like the os
// package on Linux, records with a zero inode are kept: some filesystems
// return valid files with zero inodes.
const (
	direntReclenOffset = unsafe.Offsetof(unix.Dirent{}.Reclen)
	direntNameOffset   = unsafe.Offsetof(unix.Dirent{}.Name)
)

// processIDs lists /proc, which holds "a numerical subdirectory for each
// running process" (proc_pid(5)).
func processIDs() ([]int, error) {
	return listPIDs("/proc")
}

// listPIDs returns the PIDs named by the numeric entries of a procfs-shaped
// directory, in directory order. It parses getdents64 records in place, so a
// non-numeric entry costs no allocation: unlike os.ReadDir and
// (*os.File).Readdirnames, which build a string for every entry, and
// strconv.Atoi, which allocates an error for every rejected name.
func listPIDs(dir string) ([]int, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("enumerating procfs processes: %w", err)
	}
	pids, readErr := readPIDs(f)
	if err := errors.Join(readErr, f.Close()); err != nil {
		return nil, fmt.Errorf("enumerating procfs processes: %w", err)
	}
	return pids, nil
}

// readPIDs reads every directory record of f through getdents64.
func readPIDs(f *os.File) ([]int, error) {
	conn, err := f.SyscallConn()
	if err != nil {
		return nil, err
	}
	buf := make([]byte, direntBufferSize)
	var pids []int
	var readErr error
	err = conn.Read(func(fd uintptr) bool {
		for {
			n, err := unix.Getdents(int(fd), buf)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				readErr = fmt.Errorf("getdents64: %w", err)
				return true
			}
			if n == 0 {
				return true
			}
			if pids, readErr = appendDirentPIDs(pids, buf[:n]); readErr != nil {
				return true
			}
		}
	})
	if err := errors.Join(err, readErr); err != nil {
		return nil, err
	}
	return pids, nil
}

// appendDirentPIDs appends the PID of every numeric entry among the
// getdents64 records in buf.
func appendDirentPIDs(pids []int, buf []byte) ([]int, error) {
	for len(buf) > 0 {
		if len(buf) < int(direntNameOffset) {
			return pids, fmt.Errorf("getdents64 returned a truncated %d-byte record", len(buf))
		}
		reclen := int(binary.NativeEndian.Uint16(buf[direntReclenOffset:]))
		if reclen <= int(direntNameOffset) || reclen > len(buf) {
			return pids, fmt.Errorf("getdents64 returned a %d-byte record in %d bytes", reclen, len(buf))
		}
		rec := buf[:reclen]
		buf = buf[reclen:]
		name := rec[direntNameOffset:]
		if end := bytes.IndexByte(name, 0); end >= 0 {
			name = name[:end]
		}
		if pid, ok := parsePID(name); ok {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// parsePID parses name as a positive decimal pid_t without allocating.
func parsePID(name []byte) (int, bool) {
	if len(name) == 0 {
		return 0, false
	}
	pid := 0
	for _, c := range name {
		if c < '0' || c > '9' {
			return 0, false
		}
		digit := int(c - '0')
		// Checked before multiplying so a 32-bit int cannot overflow.
		if pid > (math.MaxInt32-digit)/10 {
			return 0, false
		}
		pid = pid*10 + digit
	}
	return pid, pid > 0
}
