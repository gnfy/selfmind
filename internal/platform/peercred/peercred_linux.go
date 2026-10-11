//go:build linux

package peercred

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

// socketPeer reads SO_PEERCRED, which the kernel captures at connect time and
// translates into the daemon's pid namespace.
func socketPeer(fd int) (Credentials, error) {
	cred, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return Credentials{}, fmt.Errorf("SO_PEERCRED: %w", err)
	}
	return Credentials{UID: int(cred.Uid), PID: int(cred.Pid)}, nil
}

func lookupProcess(pid int) (process, error) {
	stat, err := readProcStat(pid)
	if err != nil {
		return process{}, err
	}
	return process{PPID: stat.PPID, PGID: stat.PGID}, nil
}

func readProcStat(pid int) (procStat, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return procStat{}, err
	}
	return parseProcStat(data)
}

// EnableChildSubreaper makes the daemon the reaper of every orphaned
// descendant. A tool that double-forks then stays the daemon's descendant
// instead of being reparented to init, so the ancestry walk still refuses it.
// The caller must also run ReapAdoptedOrphans: adopted children exit as the
// daemon's zombies.
func EnableChildSubreaper() (bool, error) {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return false, fmt.Errorf("PR_SET_CHILD_SUBREAPER: %w", err)
	}
	return true, nil
}

// adoptedZombieAge is how long a zombie child must persist before the reaper
// takes it. os/exec reaps the children it started as soon as their Wait runs;
// a pipe held open by a grandchild can delay that Wait, so the reaper leaves
// recent zombies alone and never touches a tracked tool group leader.
const adoptedZombieAge = 5 * time.Minute

// ReapAdoptedOrphans reaps zombie children the daemon adopted as a subreaper.
// It skips the leaders of tracked tool groups, whose own Wait reaps them, and
// any zombie younger than adoptedZombieAge, so it never steals an exit status
// os/exec is about to collect.
func ReapAdoptedOrphans(ctx context.Context, interval time.Duration, groups *GroupTracker) {
	self := os.Getpid()
	firstSeen := map[int]time.Time{}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			reapAdoptedOnce(self, now, firstSeen, groups)
		}
	}
}

func reapAdoptedOnce(self int, now time.Time, firstSeen map[int]time.Time, groups *GroupTracker) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	current := map[int]bool{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		stat, err := readProcStat(pid)
		if err != nil || stat.PPID != self || stat.State != 'Z' || groups.leads(pid) {
			continue
		}
		current[pid] = true
		seen, ok := firstSeen[pid]
		if !ok {
			firstSeen[pid] = now
			continue
		}
		if now.Sub(seen) < adoptedZombieAge {
			continue
		}
		var status unix.WaitStatus
		_, _ = unix.Wait4(pid, &status, unix.WNOHANG, nil)
	}
	for pid := range firstSeen {
		if !current[pid] {
			delete(firstSeen, pid)
		}
	}
}
