package process

import (
	"fmt"
	"strconv"
	"strings"
)

// Field positions are the numbered fields in proc_pid_stat(5), not indexes into
// strings.Fields: comm can itself contain whitespace and closing parentheses.
const (
	statState      = 3
	statParent     = 4
	statGroup      = 5
	statSession    = 6
	statForeground = 8
	statUserTime   = 14
	statSystemTime = 15
	statThreads    = 20
	statStart      = 22
	statVirtual    = 23
	statResident   = 24
)

type procStat struct {
	name, state                                 string
	parent, group, session, foreground, threads int
	user, system, start, virtual, resident      uint64
}

func parseStat(raw string) (procStat, error) {
	var stat procStat
	begin, end := strings.IndexByte(raw, '('), strings.LastIndexByte(raw, ')')
	if begin < 0 || end < begin {
		return stat, fmt.Errorf("proc stat: missing comm delimiters")
	}
	stat.name = raw[begin+1 : end]
	fields := strings.Fields(raw[end+1:])
	if len(fields) <= statResident-statState {
		return stat, fmt.Errorf("proc stat: truncated fields")
	}
	stat.state = fields[0]
	ints := []struct {
		field  int
		target *int
	}{
		{statParent, &stat.parent}, {statGroup, &stat.group}, {statSession, &stat.session}, {statForeground, &stat.foreground}, {statThreads, &stat.threads},
	}
	for _, entry := range ints {
		value, err := strconv.Atoi(fields[entry.field-statState])
		if err != nil {
			return stat, fmt.Errorf("proc stat field %d: %w", entry.field, err)
		}
		*entry.target = value
	}
	uints := []struct {
		field  int
		target *uint64
	}{
		{statUserTime, &stat.user}, {statSystemTime, &stat.system}, {statStart, &stat.start}, {statVirtual, &stat.virtual}, {statResident, &stat.resident},
	}
	for _, entry := range uints {
		value, err := strconv.ParseUint(fields[entry.field-statState], 10, 64)
		if err != nil {
			return stat, fmt.Errorf("proc stat field %d: %w", entry.field, err)
		}
		*entry.target = value
	}
	return stat, nil
}
