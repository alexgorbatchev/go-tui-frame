package process

import (
	"fmt"

	"golang.org/x/sys/unix"
)

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
