package process

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// processIDs lists every host process, live or zombie, through sysctl
// kern.proc.all, although that copies a whole kinfo_proc row per process.
// sysctl reports a short buffer as ENOMEM (xnu-12377.121.6
// bsd/kern/kern_sysctl.c:935-938), and SysctlKinfoProcSlice retries on it, so
// a listing never silently omits a process. libproc's proc_listallpids copies
// only PIDs but cannot promise that: the kernel caps its list at nprocs+20,
// read before it takes the process-list lock (xnu-12377.121.6
// bsd/kern/proc_info.c:388-401), and stops without error when the cap is
// reached. allproc is newest-first (xnu-12377.121.6
// bsd/kern/kern_proc.c:2930), so more than 20 processes created in that window
// would drop the oldest processes, which may include long-lived session
// members. Reusing a buffer across calls removes the allocation but not the
// time, which the kernel copy dominates.
func processIDs() ([]int, error) {
	rows, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, fmt.Errorf("enumerating native processes: %w", err)
	}
	pids := make([]int, 0, len(rows))
	for _, row := range rows {
		if row.Proc.P_pid > 0 {
			pids = append(pids, int(row.Proc.P_pid))
		}
	}
	return pids, nil
}
