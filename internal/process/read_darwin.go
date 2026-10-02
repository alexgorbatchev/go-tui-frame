package process

/*
#cgo LDFLAGS: -lproc
#include <libproc.h>
#include <sys/proc_info.h>
#include <mach/mach_time.h>
*/
import "C"

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func procInfo(pid, flavor int, dst unsafe.Pointer, size uintptr) error {
	n, err := C.proc_pidinfo(C.int(pid), C.int(flavor), 0, dst, C.int(size))
	if n == C.int(size) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("proc_pidinfo(%d,%d): %w", pid, flavor, err)
	}
	return fmt.Errorf("proc_pidinfo(%d,%d): received %d bytes, want %d", pid, flavor, n, size)
}

func startIdentity(v *C.struct_proc_bsdinfo) string {
	return fmt.Sprintf("%d:%d", v.pbi_start_tvsec, v.pbi_start_tvusec)
}

// Read samples native libproc/sysctl data without executing helper programs.
func Read(pid int) Snapshot {
	if pid <= 0 {
		return missing(pid, fmt.Errorf("reading process %d: invalid PID", pid))
	}
	var info C.struct_proc_bsdinfo
	if err := procInfo(pid, C.PROC_PIDTBSDINFO, unsafe.Pointer(&info), unsafe.Sizeof(info)); err != nil {
		return missing(pid, err)
	}
	s := missing(pid, ErrUnavailable)
	s.ParentPID = available(int(info.pbi_ppid), "libproc PROC_PIDTBSDINFO")
	s.Name = available(cString(&info.pbi_comm[0], len(info.pbi_comm)), "libproc pbi_comm (native truncated name)")
	s.State = available(strconv.FormatUint(uint64(info.pbi_status), 10), "libproc pbi_status (native Darwin state code)")
	s.StartIdentity = available(startIdentity(&info), "libproc start timeval (seconds:microseconds)")
	if info.pbi_flags&C.PROC_FLAG_CTTY != 0 && info.e_tpgid > 0 {
		s.ForegroundGroup = available(int(info.e_tpgid), "libproc controlling-terminal foreground group")
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
	readPaths(pid, &s)
	readArgs(pid, info.pbi_flags&C.PROC_FLAG_LP64 != 0, &s)
	s.Resources = readResources(pid)
	var end C.struct_proc_bsdinfo
	if err := procInfo(pid, C.PROC_PIDTBSDINFO, unsafe.Pointer(&end), unsafe.Sizeof(end)); err != nil {
		return missing(pid, err)
	}
	if startIdentity(&info) != startIdentity(&end) {
		return missing(pid, ErrIdentityChanged)
	}
	return s
}

func readPaths(pid int, s *Snapshot) {
	var paths C.struct_proc_vnodepathinfo
	const cwdSource = "libproc PROC_PIDVNODEPATHINFO current process directory"
	if err := procInfo(pid, C.PROC_PIDVNODEPATHINFO, unsafe.Pointer(&paths), unsafe.Sizeof(paths)); err != nil {
		s.Directory = unavailable[string](cwdSource, err)
	} else {
		s.Directory = available(cString(&paths.pvi_cdir.vip_path[0], len(paths.pvi_cdir.vip_path)), cwdSource)
	}
	var path [C.PROC_PIDPATHINFO_MAXSIZE]C.char
	n, err := C.proc_pidpath(C.int(pid), unsafe.Pointer(&path[0]), C.uint32_t(len(path)))
	const exeSource = "libproc proc_pidpath"
	if n <= 0 || int(n) > len(path) {
		if err == nil {
			err = ErrUnavailable
		}
		s.Executable = unavailable[string](exeSource, err)
	} else {
		s.Executable = available(C.GoStringN(&path[0], n), exeSource)
	}
}

func cString(p *C.char, n int) string {
	value, _, _ := strings.Cut(C.GoStringN(p, C.int(n)), "\x00")
	return value
}

func readArgs(pid int, wide bool, s *Snapshot) {
	const source = "sysctl kern.procargs2 kernel argument/environment view; environment may be omitted by platform policy"
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		s.Args = unavailable[[]string](source, err)
		s.Environment = unavailable[[]string](source, err)
		return
	}
	pointerBytes := 4
	if wide {
		pointerBytes = 8
	}
	args, env, err := parseArgs(raw, pointerBytes)
	if err != nil {
		s.Args = unavailable[[]string](source, err)
		s.Environment = unavailable[[]string](source, err)
		return
	}
	s.Args = available(args, source)
	if len(env) == 0 {
		s.Environment = unavailable[[]string](source, ErrUnavailable)
	} else {
		s.Environment = available(env, source)
	}
}

func parseArgs(raw []byte, pointerBytes int) ([]string, []string, error) {
	const argcBytes = 4 // XNU returns a native 32-bit int before the strings.
	if len(raw) < argcBytes {
		return nil, nil, fmt.Errorf("procargs2: missing argc")
	}
	argc := int(int32(binary.NativeEndian.Uint32(raw[:argcBytes])))
	if argc < 0 || argc > len(raw) {
		return nil, nil, fmt.Errorf("procargs2: invalid argc %d", argc)
	}
	raw = raw[argcBytes:]
	end := bytes.IndexByte(raw, 0)
	if end < 0 {
		return nil, nil, fmt.Errorf("procargs2: missing executable terminator")
	}
	// XNU aligns the original executable_path= string to the target pointer
	// width, then strips that key in procargs2. Skipping every NUL would also
	// skip legitimate empty argv entries and consume environment as arguments.
	const executableKey = "executable_path="
	pathBytes := len(executableKey) + end + 1
	padding := (pointerBytes - pathBytes%pointerBytes) % pointerBytes
	raw = raw[end+1:]
	if len(raw) < padding {
		return nil, nil, fmt.Errorf("procargs2: truncated executable alignment")
	}
	for _, value := range raw[:padding] {
		if value != 0 {
			return nil, nil, fmt.Errorf("procargs2: unexpected executable alignment")
		}
	}
	raw = raw[padding:]
	args := make([]string, 0, argc)
	for range argc {
		end = bytes.IndexByte(raw, 0)
		if end < 0 {
			return nil, nil, fmt.Errorf("procargs2: truncated arguments")
		}
		args = append(args, string(raw[:end]))
		raw = raw[end+1:]
	}
	var env []string
	for len(raw) > 0 {
		end = bytes.IndexByte(raw, 0)
		if end < 0 {
			return args, nil, fmt.Errorf("procargs2: truncated environment")
		}
		if end == 0 {
			break
		}
		env = append(env, string(raw[:end]))
		raw = raw[end+1:]
	}
	return args, env, nil
}

func readResources(pid int) Resources {
	const source = "libproc PROC_PIDTASKINFO"
	var info C.struct_proc_taskinfo
	if err := procInfo(pid, C.PROC_PIDTASKINFO, unsafe.Pointer(&info), unsafe.Sizeof(info)); err != nil {
		return missing(pid, err).Resources
	}
	r := Resources{
		ResidentBytes: available(uint64(info.pti_resident_size), source+" resident-size bytes"),
		VirtualBytes:  available(uint64(info.pti_virtual_size), source+" virtual-size bytes"),
		Threads:       available(int(info.pti_threadnum), source),
	}
	var base C.mach_timebase_info_data_t
	if code := C.mach_timebase_info(&base); code != 0 || base.denom == 0 {
		err := fmt.Errorf("mach_timebase_info: code %d, denominator %d", code, base.denom)
		r.CPUUser = unavailable[time.Duration](source, err)
		r.CPUSystem = unavailable[time.Duration](source, err)
	} else {
		r.CPUUser = scaledDuration(uint64(info.pti_total_user), uint64(base.numer), uint64(base.denom), source+" user Mach timebase ticks")
		r.CPUSystem = scaledDuration(uint64(info.pti_total_system), uint64(base.numer), uint64(base.denom), source+" system Mach timebase ticks")
	}
	r.Native = available(map[string]uint64{
		"user_mach_ticks": uint64(info.pti_total_user), "system_mach_ticks": uint64(info.pti_total_system),
		"threads_user_mach_ticks": uint64(info.pti_threads_user), "threads_system_mach_ticks": uint64(info.pti_threads_system),
		"page_faults": uint64(info.pti_faults), "pageins": uint64(info.pti_pageins), "copy_on_write_faults": uint64(info.pti_cow_faults),
		"messages_sent": uint64(info.pti_messages_sent), "messages_received": uint64(info.pti_messages_received),
		"mach_syscalls": uint64(info.pti_syscalls_mach), "unix_syscalls": uint64(info.pti_syscalls_unix),
		"context_switches": uint64(info.pti_csw), "running_threads": uint64(info.pti_numrunning),
	}, source)
	return r
}
