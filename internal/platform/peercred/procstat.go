package peercred

import (
	"bytes"
	"fmt"
	"strconv"
)

// procStat is the part of /proc/<pid>/stat the walk and the reaper use.
type procStat struct {
	State byte
	PPID  int
	PGID  int
}

// parseProcStat parses "pid (comm) state ppid pgrp ...". The command name may
// contain spaces and parentheses, so fields are read after the LAST ')'.
func parseProcStat(data []byte) (procStat, error) {
	end := bytes.LastIndexByte(data, ')')
	if end < 0 || end+2 > len(data) {
		return procStat{}, fmt.Errorf("malformed stat line")
	}
	fields := bytes.Fields(data[end+1:])
	if len(fields) < 3 || len(fields[0]) != 1 {
		return procStat{}, fmt.Errorf("malformed stat fields")
	}
	ppid, err := strconv.Atoi(string(fields[1]))
	if err != nil {
		return procStat{}, fmt.Errorf("malformed parent pid: %w", err)
	}
	pgid, err := strconv.Atoi(string(fields[2]))
	if err != nil {
		return procStat{}, fmt.Errorf("malformed process group: %w", err)
	}
	return procStat{State: fields[0][0], PPID: ppid, PGID: pgid}, nil
}
