package process

import (
	"fmt"
	"os"
	"strconv"
)

func processIDs() ([]int, error) {
	rows, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("enumerating procfs processes: %w", err)
	}
	pids := make([]int, 0, len(rows))
	for _, row := range rows {
		pid, err := strconv.Atoi(row.Name())
		if err == nil && pid > 0 && row.IsDir() {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}
