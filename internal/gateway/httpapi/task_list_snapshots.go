package httpapi

import (
	"strings"
	"sync"
	"time"

	"selfmind/internal/control"
)

const taskListSnapshotTTL = 30 * time.Minute

type taskListSnapshot struct {
	taskIDs   []string
	expiresAt time.Time
}

// taskListSnapshotRegistry is deliberately process-local: numbered cards are
// channel-local presentation state, not durable work state. A daemon restart
// safely falls back to the current ranked list; stable task ids remain the
// cross-restart and automation surface.
type taskListSnapshotRegistry struct {
	mu      sync.Mutex
	entries map[string]taskListSnapshot
}

func taskListSnapshotKey(identity *control.IdentityContext, channel string) string {
	if identity == nil {
		return ""
	}
	return strings.Join([]string{
		strings.TrimSpace(identity.TenantID),
		strings.TrimSpace(identity.PersonID),
		strings.TrimSpace(identity.AccountID),
		strings.TrimSpace(channel),
	}, "\x00")
}

func (r *taskListSnapshotRegistry) remember(identity *control.IdentityContext, channel string, tasks []control.Task, now time.Time) {
	key := taskListSnapshotKey(identity, channel)
	if key == "" {
		return
	}
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		if id := strings.TrimSpace(task.ID); id != "" {
			ids = append(ids, id)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = make(map[string]taskListSnapshot)
	}
	for existingKey, snapshot := range r.entries {
		if !now.Before(snapshot.expiresAt) {
			delete(r.entries, existingKey)
		}
	}
	r.entries[key] = taskListSnapshot{taskIDs: ids, expiresAt: now.Add(taskListSnapshotTTL)}
}

// resolve returns (task id, displayed count, snapshot found). A found snapshot
// with an empty id means the requested ordinal was outside that exact list; it
// must not silently fall through to a newly ordered live query.
func (r *taskListSnapshotRegistry) resolve(identity *control.IdentityContext, channel string, ordinal int, now time.Time) (string, int, bool) {
	key := taskListSnapshotKey(identity, channel)
	if key == "" {
		return "", 0, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	snapshot, ok := r.entries[key]
	if !ok {
		return "", 0, false
	}
	if !now.Before(snapshot.expiresAt) {
		delete(r.entries, key)
		return "", 0, false
	}
	if ordinal < 1 || ordinal > len(snapshot.taskIDs) {
		return "", len(snapshot.taskIDs), true
	}
	return snapshot.taskIDs[ordinal-1], len(snapshot.taskIDs), true
}
