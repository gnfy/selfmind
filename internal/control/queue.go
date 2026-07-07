package control

// Durable per-person task queue (G1+G2). When a run is already active for a
// person, genuinely new (non-continuation) work is enqueued here instead of
// being rejected as "busy". Rows survive a daemon restart so a queued task
// pending at crash time still runs on the next boot drain. Queueing is the
// gateway's per-person serialization; a drained item becomes a normal async
// run, so the worker pool still schedules it.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Queue row lifecycle: queued -> started (drained into an async run) -> done
// (the drained run finalized); or queued -> cancelled (/queue clear). "started"
// is durable so a boot drain can tell which rows were mid-launch when the daemon
// died and requeue them; "done" is terminal so a COMPLETED drained item is never
// re-run at the next boot (the duplicate-execution bug: a started row that
// finalized normally was still requeued and re-ran the completed work).
const (
	QueueStatusQueued    = "queued"
	QueueStatusStarted   = "started"
	QueueStatusDone      = "done"
	QueueStatusCancelled = "cancelled"
)

// QueuedTask is one deferred piece of new work for a person. It carries the
// minimum an async run needs to route its result back to the origin endpoint
// (channel/platform/platform_user_id) and to reproduce the request scope
// (approval_mode, workspace_id).
type QueuedTask struct {
	ID             string    `json:"id"`
	TenantID       string    `json:"tenant_id"`
	PersonID       string    `json:"person_id"`
	Channel        string    `json:"channel"`
	Platform       string    `json:"platform"`
	PlatformUserID string    `json:"platform_user_id,omitempty"`
	Content        string    `json:"content"`
	ApprovalMode   string    `json:"approval_mode,omitempty"`
	WorkspaceID    string    `json:"workspace_id,omitempty"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"created_at"`
}

// EnqueueQueued appends a new queued task for a person and returns the stored
// row. Content and person are required; the rest is best-effort routing state.
func (s *Store) EnqueueQueued(ctx context.Context, q QueuedTask) (*QueuedTask, error) {
	q.TenantID = normalizeTenant(q.TenantID)
	if strings.TrimSpace(q.PersonID) == "" {
		return nil, fmt.Errorf("person id is required")
	}
	if strings.TrimSpace(q.Content) == "" {
		return nil, fmt.Errorf("content is required")
	}
	q.Platform = normalizeName(q.Platform, "cli")
	q.Channel = normalizeName(q.Channel, q.Platform)
	if q.ID == "" {
		q.ID = "queue_" + uuid.NewString()
	}
	q.Status = QueueStatusQueued
	q.CreatedAt = time.Now()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO task_queue (id, tenant_id, person_id, channel, platform, platform_user_id, content, approval_mode, workspace_id, status, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		q.ID, q.TenantID, q.PersonID, q.Channel, q.Platform, q.PlatformUserID, q.Content, q.ApprovalMode, q.WorkspaceID, q.Status, q.CreatedAt.Unix())
	if err != nil {
		return nil, err
	}
	return &q, nil
}

func scanQueuedTask(rows interface {
	Scan(dest ...interface{}) error
}) (QueuedTask, error) {
	var q QueuedTask
	var created int64
	if err := rows.Scan(&q.ID, &q.TenantID, &q.PersonID, &q.Channel, &q.Platform, &q.PlatformUserID,
		&q.Content, &q.ApprovalMode, &q.WorkspaceID, &q.Status, &created); err != nil {
		return QueuedTask{}, err
	}
	q.CreatedAt = time.Unix(created, 0)
	return q, nil
}

const queueSelectColumns = `id, tenant_id, person_id, channel, platform, COALESCE(platform_user_id, ''),
	content, COALESCE(approval_mode, ''), COALESCE(workspace_id, ''), status, created_at`

// ListQueued returns a person's queue rows in FIFO order (oldest first) for the
// given status. An empty status defaults to "queued".
func (s *Store) ListQueued(ctx context.Context, tenantID, personID, status string) ([]QueuedTask, error) {
	if strings.TrimSpace(personID) == "" {
		return nil, fmt.Errorf("person id is required")
	}
	status = normalizeName(status, QueueStatusQueued)
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+queueSelectColumns+`
		 FROM task_queue WHERE tenant_id = ? AND person_id = ? AND status = ?
		 ORDER BY created_at ASC, rowid ASC`,
		normalizeTenant(tenantID), personID, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueuedTask
	for rows.Next() {
		q, err := scanQueuedTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// NextQueued returns the oldest still-queued task for a person, or nil when the
// queue is empty. It never mutates state; the caller marks the row started only
// once it has committed to launching it.
func (s *Store) NextQueued(ctx context.Context, tenantID, personID string) (*QueuedTask, error) {
	if strings.TrimSpace(personID) == "" {
		return nil, fmt.Errorf("person id is required")
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+queueSelectColumns+`
		 FROM task_queue WHERE tenant_id = ? AND person_id = ? AND status = ?
		 ORDER BY created_at ASC, rowid ASC LIMIT 1`,
		normalizeTenant(tenantID), personID, QueueStatusQueued)
	q, err := scanQueuedTask(row)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return nil, nil
		}
		return nil, err
	}
	return &q, nil
}

// GetQueued fetches one queue row by id (any status). Diagnostic/test helper.
func (s *Store) GetQueued(ctx context.Context, tenantID, id string) (*QueuedTask, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+queueSelectColumns+`
		 FROM task_queue WHERE tenant_id = ? AND id = ?`,
		normalizeTenant(tenantID), id)
	q, err := scanQueuedTask(row)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return nil, nil
		}
		return nil, err
	}
	return &q, nil
}

// CountQueued returns how many rows a person has in the given status. Used for
// the "N ahead" acceptance line and /queue.
func (s *Store) CountQueued(ctx context.Context, tenantID, personID, status string) (int, error) {
	if strings.TrimSpace(personID) == "" {
		return 0, fmt.Errorf("person id is required")
	}
	status = normalizeName(status, QueueStatusQueued)
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM task_queue WHERE tenant_id = ? AND person_id = ? AND status = ?`,
		normalizeTenant(tenantID), personID, status).Scan(&n)
	return n, err
}

// MarkQueued transitions one queue row to a new status (started/cancelled, or
// back to queued when a launch races and must be reverted).
func (s *Store) MarkQueued(ctx context.Context, tenantID, id, status string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("queue id is required")
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE task_queue SET status = ? WHERE tenant_id = ? AND id = ?`,
		normalizeName(status, QueueStatusQueued), normalizeTenant(tenantID), id)
	return err
}

// ClearQueued cancels every still-queued row for a person and returns how many
// were dropped. Backs `/queue clear`.
func (s *Store) ClearQueued(ctx context.Context, tenantID, personID string) (int, error) {
	if strings.TrimSpace(personID) == "" {
		return 0, fmt.Errorf("person id is required")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE task_queue SET status = ? WHERE tenant_id = ? AND person_id = ? AND status = ?`,
		QueueStatusCancelled, normalizeTenant(tenantID), personID, QueueStatusQueued)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ListAllQueued returns queued rows across all persons/tenants for the given
// status, oldest first. The boot drain uses it to find every person with
// pending work after a restart.
func (s *Store) ListAllQueued(ctx context.Context, status string) ([]QueuedTask, error) {
	status = normalizeName(status, QueueStatusQueued)
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+queueSelectColumns+`
		 FROM task_queue WHERE status = ? ORDER BY created_at ASC, rowid ASC`,
		status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueuedTask
	for rows.Next() {
		q, err := scanQueuedTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// maxQueueRestarts bounds how many boots may resurrect one 'started' row. A
// row left 'started' usually means the daemon died mid-run; ONE retry is owed.
// Unbounded retries turn a long task + frequent restarts into an infinite
// resurrection loop (observed live: five duplicate "tank game" task corpses
// after a day of deploy restarts).
const maxQueueRestarts = 1

// RequeueStartedQueued flips 'started' rows back to 'queued' (boot recovery:
// the daemon died between marking a row started and its run finalizing) and
// returns the requeued count. Rows that already used their restart budget are
// marked failed instead — never silently, the count of dropped rows is
// returned too. Safe at boot: gateway.lock guarantees single ownership.
func (s *Store) RequeueStartedQueued(ctx context.Context) (requeued, dropped int, err error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE task_queue SET status = ?, restarts = restarts + 1
		 WHERE status = ? AND restarts < ?`,
		QueueStatusQueued, QueueStatusStarted, maxQueueRestarts)
	if err != nil {
		return 0, 0, err
	}
	nRequeued, _ := res.RowsAffected()
	res, err = s.db.ExecContext(ctx,
		`UPDATE task_queue SET status = 'failed' WHERE status = ?`,
		QueueStatusStarted)
	if err != nil {
		return int(nRequeued), 0, err
	}
	nDropped, _ := res.RowsAffected()
	return int(nRequeued), int(nDropped), nil
}
