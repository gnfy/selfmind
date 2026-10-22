// Package peercred identifies the process at the other end of a local Unix
// domain socket and decides whether it may act as the person's own CLI.
//
// The daemon and every tool it runs share one OS user, so a same-user check
// alone cannot tell the person's terminal from a command the model started.
// The verdict therefore also requires that the peer is not the daemon or one
// of its descendants, and not a member of a process group the daemon started
// for a tool. The kernel reports the peer's credentials at connect time; no
// value the peer sends can change them.
package peercred

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// Credentials are the kernel-reported identity of a connected peer.
type Credentials struct {
	UID int
	PID int
}

// ErrUnsupported reports a platform without peer credentials.
var ErrUnsupported = errors.New("peer credentials are not supported on this platform")

// Of returns the credentials of the process that connected conn.
func Of(conn net.Conn) (Credentials, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return Credentials{}, fmt.Errorf("peer credentials need a Unix domain socket, got %T", conn)
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return Credentials{}, err
	}
	var creds Credentials
	var lookupErr error
	if err := raw.Control(func(fd uintptr) {
		creds, lookupErr = socketPeer(int(fd))
	}); err != nil {
		return Credentials{}, err
	}
	return creds, lookupErr
}

// Verdict is the daemon's decision about one connection.
type Verdict struct {
	Verified bool
	UID      int
	PID      int
	// Reason explains a refusal in words safe to show the person.
	Reason string
}

// process is the part of a process table entry the ancestry walk needs.
type process struct {
	PPID int
	PGID int
}

// maxAncestry bounds the walk; real process trees are a few dozen deep.
const maxAncestry = 4096

// Verifier decides whether a peer is the daemon owner's own process outside
// the daemon's process tree.
type Verifier struct {
	DaemonPID int
	OwnerUID  int
	// Groups are process groups the daemon started for tools. Optional.
	Groups *GroupTracker
	// lookup reads one process table entry; nil uses the host's.
	lookup func(pid int) (process, error)
}

// Check returns the verdict for the process that connected conn.
func (v Verifier) Check(conn net.Conn) Verdict {
	creds, err := Of(conn)
	if err != nil {
		return Verdict{Reason: "peer credentials unavailable: " + err.Error()}
	}
	return v.CheckCredentials(creds)
}

// CheckCredentials applies the verdict to kernel-reported credentials.
func (v Verifier) CheckCredentials(creds Credentials) Verdict {
	verdict := Verdict{UID: creds.UID, PID: creds.PID}
	if creds.UID != v.OwnerUID {
		verdict.Reason = fmt.Sprintf("peer runs as uid %d, not the daemon owner", creds.UID)
		return verdict
	}
	if creds.PID <= 0 {
		verdict.Reason = "peer process id unavailable"
		return verdict
	}
	lookup := v.lookup
	if lookup == nil {
		lookup = lookupProcess
	}
	pid := creds.PID
	for steps := 0; ; steps++ {
		if pid == v.DaemonPID {
			verdict.Reason = "peer is the daemon or a process it started"
			return verdict
		}
		if steps >= maxAncestry {
			verdict.Reason = "peer process ancestry is too deep to verify"
			return verdict
		}
		entry, err := lookup(pid)
		if err != nil {
			// An ancestor the daemon cannot read could hide the daemon above it.
			verdict.Reason = fmt.Sprintf("peer process ancestry unavailable at pid %d: %v", pid, err)
			return verdict
		}
		if v.Groups.Contains(entry.PGID) {
			verdict.Reason = "peer belongs to a process group the daemon started for a tool"
			return verdict
		}
		if entry.PPID <= 0 || entry.PPID == pid || pid == 1 {
			break
		}
		pid = entry.PPID
	}
	verdict.Verified = true
	return verdict
}

// GroupTracker remembers the process groups of tool commands for as long as
// any member of the group is alive.
type GroupTracker struct {
	mu     sync.Mutex
	groups map[int]struct{}
	alive  func(pgid int) bool
}

// NewGroupTracker returns an empty tracker.
func NewGroupTracker() *GroupTracker {
	return &GroupTracker{groups: make(map[int]struct{}), alive: groupAlive}
}

// Track records a process group led by a tool command.
func (t *GroupTracker) Track(pgid int) {
	if t == nil || pgid <= 1 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked()
	t.groups[pgid] = struct{}{}
}

// Contains reports whether pgid is a live tool process group. Dead groups are
// forgotten on every call, so a recycled id is not mistaken for a tool's.
func (t *GroupTracker) Contains(pgid int) bool {
	if t == nil || pgid <= 1 {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked()
	_, ok := t.groups[pgid]
	return ok
}

// leads reports whether pid leads a tracked group, without pruning: a zombie
// leader still holds its group id until it is reaped.
func (t *GroupTracker) leads(pid int) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.groups[pid]
	return ok
}

func (t *GroupTracker) pruneLocked() {
	for pgid := range t.groups {
		if !t.alive(pgid) {
			delete(t.groups, pgid)
		}
	}
}

// PruneEvery forgets dead groups on a timer until ctx ends. Without it a group
// is forgotten only when someone next asks, and a group id that was recycled to
// a new owner in between still looks alive, so it would be mistaken for a tool's.
func PruneEvery(ctx context.Context, tracker *GroupTracker, interval time.Duration) {
	if tracker == nil || interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tracker.mu.Lock()
			tracker.pruneLocked()
			tracker.mu.Unlock()
		}
	}
}
