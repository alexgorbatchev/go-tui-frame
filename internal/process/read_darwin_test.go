package process

import (
	"fmt"
	"os"
	"regexp"
	"runtime"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Read calls proc_pidinfo for the BSD info, the vnode paths, the task info and
// the closing identity check, plus proc_pidpath.
const readCgoCalls = 5

// readConstantAllocs is the number of allocations a Darwin Read makes beyond
// fetching and splitting kern.procargs2, whose count follows the argument and
// environment vectors.
const readConstantAllocs = 14

var startIdentityFormat = regexp.MustCompile(`^[0-9]+:[0-9]+$`)

func wantReadSources(int) map[string]string {
	const args = "sysctl kern.procargs2 kernel argument/environment view; environment may be omitted by platform policy"
	return map[string]string{
		"ParentPID": "libproc PROC_PIDTBSDINFO", "SessionID": "getsid", "ProcessGroup": "getpgid",
		"Name": "libproc pbi_comm (native truncated name)", "State": "libproc pbi_status (native Darwin state code)",
		"Directory": "libproc PROC_PIDVNODEPATHINFO current process directory", "Executable": "libproc proc_pidpath",
		"Args": args, "Environment": args, "StartIdentity": "libproc start timeval (seconds:microseconds)",
		"Status":        "native process status",
		"ResidentBytes": "libproc PROC_PIDTASKINFO resident-size bytes", "VirtualBytes": "libproc PROC_PIDTASKINFO virtual-size bytes",
		"CPUUser":   "libproc PROC_PIDTASKINFO user Mach timebase ticks converted to nanoseconds",
		"CPUSystem": "libproc PROC_PIDTASKINFO system Mach timebase ticks converted to nanoseconds",
		"Threads":   "libproc PROC_PIDTASKINFO", "Native": "libproc PROC_PIDTASKINFO",
	}
}

// nativeStartIdentity reads the start timeval through sysctl kern.proc.pid,
// independently of libproc.
func nativeStartIdentity(t *testing.T, pid int) string {
	t.Helper()
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%d:%d", info.Proc.P_starttime.Sec, info.Proc.P_starttime.Usec)
}

func TestReadAllocatesOnlyPerProcessValues(t *testing.T) {
	pid := os.Getpid()
	Read(pid)
	read := testing.AllocsPerRun(50, func() { Read(pid) })
	pointerBytes := int(unsafe.Sizeof(uintptr(0)))
	vectors := testing.AllocsPerRun(50, func() {
		raw, err := unix.SysctlRaw("kern.procargs2", pid)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := parseArgs(raw, pointerBytes); err != nil {
			t.Fatal(err)
		}
	})
	if got := read - vectors; got != readConstantAllocs {
		t.Fatalf("Read makes %v allocations beyond its %v argument-vector allocations, want %d", got, vectors, readConstantAllocs)
	}
}

// libproc returns the directory in a MAXPATHLEN array; a retained Directory
// must not pin that whole array.
func TestReadDirectoryRetainsOnlyPathBytes(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	const pathMax = 1024
	if 2*len(cwd) >= pathMax {
		t.Fatalf("working directory %q is too long to tell its length from MAXPATHLEN", cwd)
	}
	pid := os.Getpid()
	const samples = 256
	kept := make([]string, samples)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := range kept {
		kept[i] = Read(pid).Directory.Value
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	retained := (int64(after.HeapAlloc) - int64(before.HeapAlloc)) / samples
	for _, dir := range kept {
		if dir != cwd {
			t.Fatalf("directory = %q, want %q", dir, cwd)
		}
	}
	if retained >= int64(2*len(cwd)) {
		t.Fatalf("each retained %d-byte directory keeps %d heap bytes", len(cwd), retained)
	}
}
