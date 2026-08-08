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
	QueueStatusFailed    = "failed"
	QueueStatusCancelled = "cancelled"
)

func normalizeQueueClass(class string) string {
	switch strings.ToLower(strings.TrimSpace(class)) {
	case QueueClassFinalization:
		return QueueClassFinalization
	case QueueClassBackground:
		return QueueClassBackground
	case QueueClassCron:
		return QueueClassCron
	default:
		return QueueClassForeground
	}
}

func queuePriorityForClass(class string) int {
	switch normalizeQueueClass(class) {
	case QueueClassFinalization:
		return QueuePriorityFinalization
	case QueueClassBackground:
		return QueuePriorityBackground
	case QueueClassCron:
		return QueuePriorityCron
	default:
		return QueuePriorityForeground
	}
}

const (
	QueueClassForeground   = "foreground"
	QueueClassFinalization = "finalization"
	QueueClassBackground   = "background"
	QueueClassCron         = "cron"
)

const (
	QueuePriorityForeground   = 100
	QueuePriorityFinalization = 80
	QueuePriorityBackground   = 50
	QueuePriorityCron         = 20
)

// QueuedTask is one deferred piece of new work for a person. It carries the
// minimum an async run needs to route its result back to the origin endpoint
// (channel/platform/platform_user_id) and to reproduce the request scope
// (approval_mode, workspace_id).
type QueuedTask struct {
	ID             string `json:"id"`
	TenantID       string `json:"tenant_id"`
	PersonID       string `json:"person_id"`
	Channel        string `json:"channel"`
	Platform       string `json:"platform"`
	PlatformUserID string `json:"platform_user_id,omitempty"`
	Content        string `json:"content"`
	ApprovalMode   string `json:"approval_mode,omitempty"`
	WorkspaceID    string `json:"workspace_id,omitempty"`
	// TaskID pins the drained run to a specific existing task (used by
	// system-originated finalization work such as external-watch closure).
	// Empty for ordinary inbound messages, which resolve their task normally.
	TaskID string `json:"task_id,omitempty"`
	// RunID is filled after StartRun succeeds. It lets recovery distinguish a
	// queue row whose run durably finalized from one that only reached the
	// queue-level done marker before a crash.
	RunID string `json:"run_id,omitempty"`
	// IdempotencyKey deduplicates system-originated enqueues across crash
	// recovery replays. Empty (ordinary inbound) rows never deduplicate.
	IdempotencyKey    string    `json:"idempotency_key,omitempty"`
	Class             string    `json:"class,omitempty"`
	Priority          int       `json:"priority,omitempty"`
	NotBefore         time.Time `json:"not_before,omitempty"`
	Status            string    `json:"status"`
	Restarts          int       `json:"restarts,omitempty"`
	ClaimToken        string    `json:"claim_token,omitempty"`
	LeaseUntil        time.Time `json:"lease_until,omitempty"`
	AttemptGeneration int       `json:"attempt_generation,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
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
	q.IdempotencyKey = strings.TrimSpace(q.IdempotencyKey)
	q.Class = normalizeQueueClass(q.Class)
	if q.Priority == 0 {
		q.Priority = queuePriorityForClass(q.Class)
	}
	notBefore := q.NotBefore.Unix()
	if q.NotBefore.IsZero() {
		notBefore = 0
	}
	query := `INSERT INTO task_queue (id, tenant_id, person_id, channel, platform, platform_user_id, content, approval_mode, workspace_id, task_id, idempotency_key, class, priority, not_before, status, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if q.IdempotencyKey != "" {
		query += ` ON CONFLICT(tenant_id, idempotency_key) WHERE idempotency_key != '' DO NOTHING`
	}
	result, err := s.db.ExecContext(ctx, query,
		q.ID, q.TenantID, q.PersonID, q.Channel, q.Platform, q.PlatformUserID, q.Content, q.ApprovalMode, q.WorkspaceID, q.TaskID, q.IdempotencyKey, q.Class, q.Priority, notBefore, q.Status, q.CreatedAt.Unix())
	if err != nil {
		return nil, err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		if q.IdempotencyKey == "" {
			return nil, fmt.Errorf("queue insert affected no rows without an idempotency key")
		}
		// The stable idempotency key already has a row: a replayed enqueue is
		// success, never an error (a recovery loop must not spin on it).
		existing := s.db.QueryRowContext(ctx,
			`SELECT `+queueSelectColumns+` FROM task_queue WHERE tenant_id = ? AND idempotency_key = ?`, q.TenantID, q.IdempotencyKey)
		dup, err := scanQueuedTask(existing)
		if err != nil {
			return nil, fmt.Errorf("load duplicate queued task %q: %w", q.IdempotencyKey, err)
		}
		return &dup, nil
	}
	return &q, nil
}

func scanQueuedTask(rows interface {
	Scan(dest ...interface{}) error
}) (QueuedTask, error) {
	var q QueuedTask
	var created, notBefore, leaseUntil int64
	if err := rows.Scan(&q.ID, &q.TenantID, &q.PersonID, &q.Channel, &q.Platform, &q.PlatformUserID,
		&q.Content, &q.ApprovalMode, &q.WorkspaceID, &q.TaskID, &q.RunID, &q.IdempotencyKey,
		&q.Class, &q.Priority, &notBefore, &q.Status, &q.Restarts, &q.ClaimToken, &leaseUntil,
		&q.AttemptGeneration, &created); err != nil {
		return QueuedTask{}, err
	}
	q.CreatedAt = time.Unix(created, 0)
	if notBefore > 0 {
		q.NotBefore = time.Unix(notBefore, 0)
	}
	if leaseUntil > 0 {
		q.LeaseUntil = time.Unix(leaseUntil, 0)
	}
	return q, nil
}

const queueSelectColumns = `id, tenant_id, person_id, channel, platform, COALESCE(platform_user_id, ''),
	content, COALESCE(approval_mode, ''), COALESCE(workspace_id, ''), COALESCE(task_id, ''),
	COALESCE(run_id, ''), COALESCE(idempotency_key, ''), COALESCE(class, 'foreground'),
	COALESCE(priority, 100), COALESCE(not_before, 0), status, COALESCE(restarts, 0),
	COALESCE(claim_token, ''), COALESCE(lease_until, 0), COALESCE(attempt_generation, 0), created_at`

const defaultQueueClaimLease = 2 * time.Minute

// ClaimQueued atomically gives one worker ownership of a due queued row. The
// token is required for binding and renewal, so a stale worker cannot extend a
// later attempt after recovery has reassigned the stable queue row.
func (s *Store) ClaimQueued(ctx context.Context, tenantID, id string, leaseFor time.Duration) (string, bool, error) {
	if strings.TrimSpace(id) == "" {
		return "", false, fmt.Errorf("queue id is required")
	}
	if leaseFor <= 0 {
		leaseFor = defaultQueueClaimLease
	}
	token := "claim_" + uuid.NewString()
	now := time.Now()
	result, err := s.db.ExecContext(ctx,
		`UPDATE task_queue SET status = ?, claim_token = ?, lease_until = ?, attempt_generation = attempt_generation + 1
		 WHERE tenant_id = ? AND id = ? AND status = ? AND COALESCE(not_before, 0) <= ?`,
		QueueStatusStarted, token, now.Add(leaseFor).Unix(), normalizeTenant(tenantID), id,
		QueueStatusQueued, now.Unix())
	if err != nil {
		return "", false, err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return "", false, nil
	}
	return token, true, nil
}

// RenewQueuedClaim keeps a live worker's ownership independent of model/tool
// latency. It is deliberately keyed by the opaque claim token, not only row id.
func (s *Store) RenewQueuedClaim(ctx context.Context, tenantID, id, token string, leaseFor time.Duration) (bool, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(token) == "" {
		return false, nil
	}
	if leaseFor <= 0 {
		leaseFor = defaultQueueClaimLease
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE task_queue SET lease_until = ?
		 WHERE tenant_id = ? AND id = ? AND status = ? AND claim_token = ?`,
		time.Now().Add(leaseFor).Unix(), normalizeTenant(tenantID), id, QueueStatusStarted, token)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n == 1, nil
}

// ListQueued returns a person's queue rows in scheduler order (priority first,
// FIFO within one priority) for the given status. An empty status defaults to
// "queued". Future not_before rows remain visible to diagnostics and /queue.
func (s *Store) ListQueued(ctx context.Context, tenantID, personID, status string) ([]QueuedTask, error) {
	if strings.TrimSpace(personID) == "" {
		return nil, fmt.Errorf("person id is required")
	}
	status = normalizeName(status, QueueStatusQueued)
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+queueSelectColumns+`
		 FROM task_queue WHERE tenant_id = ? AND person_id = ? AND status = ?
		 ORDER BY priority DESC, created_at ASC, rowid ASC`,
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

// NextQueued returns the highest-priority due task for a person, or nil when no
// row is currently due. It never mutates state; the caller marks the row
// started only once it has committed to launching it.
func (s *Store) NextQueued(ctx context.Context, tenantID, personID string) (*QueuedTask, error) {
	if strings.TrimSpace(personID) == "" {
		return nil, fmt.Errorf("person id is required")
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+queueSelectColumns+`
		 FROM task_queue WHERE tenant_id = ? AND person_id = ? AND status = ? AND COALESCE(not_before, 0) <= ?
		 ORDER BY priority DESC, created_at ASC, rowid ASC LIMIT 1`,
		normalizeTenant(tenantID), personID, QueueStatusQueued, time.Now().Unix())
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

// GetQueuedByIdempotencyKey fetches one system-originated queue row. Ordinary
// inbound rows intentionally have no idempotency key and are not addressable
// through this recovery API.
func (s *Store) GetQueuedByIdempotencyKey(ctx context.Context, tenantID, key string) (*QueuedTask, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, nil
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+queueSelectColumns+` FROM task_queue WHERE tenant_id = ? AND idempotency_key = ?`, normalizeTenant(tenantID), key)
	q, err := scanQueuedTask(row)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return nil, nil
		}
		return nil, err
	}
	return &q, nil
}

// UpdateSystemQueuedContent refreshes the execution contract of one durable
// system row without touching ordinary inbound messages. Recovery jobs can
// outlive the binary version that created them, so replay must use the current
// deterministic materialization prompt rather than stale instructions stored
// by an older daemon.
func (s *Store) UpdateSystemQueuedContent(ctx context.Context, tenantID, id, content string) (bool, error) {
	if strings.TrimSpace(id) == "" {
		return false, fmt.Errorf("queue id is required")
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE task_queue SET content = ?
		 WHERE tenant_id = ? AND id = ? AND idempotency_key != ''
		   AND status != ?`,
		content, normalizeTenant(tenantID), id, QueueStatusCancelled)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n == 1, nil
}

// RequeueSystemQueued reopens one idempotent system row after its prior launch
// ended without materializing the promised durable state. It never touches
// ordinary user messages, and the caller supplies a small hard retry budget so
// reconciliation cannot become an infinite execution loop.
func (s *Store) RequeueSystemQueued(ctx context.Context, tenantID, id string, maxRestarts int) (bool, error) {
	if strings.TrimSpace(id) == "" {
		return false, fmt.Errorf("queue id is required")
	}
	if maxRestarts < 1 {
		maxRestarts = 1
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE task_queue SET status = ?, run_id = '', restarts = restarts + 1,
			claim_token = '', lease_until = 0
		 WHERE tenant_id = ? AND id = ? AND idempotency_key != ''
		   AND (status = ? OR (status = ? AND COALESCE(lease_until, 0) <= ?))
		   AND restarts < ?`,
		QueueStatusQueued, normalizeTenant(tenantID), id,
		QueueStatusFailed, QueueStatusStarted, time.Now().Unix(), maxRestarts)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n == 1, nil
}

// BindQueuedRun records the exact run created from a started queue row. A
// different run id is rejected so two launchers cannot claim the same row.
func (s *Store) BindQueuedRun(ctx context.Context, tenantID, id, runID string) error {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(runID) == "" {
		return fmt.Errorf("queue id and run id are required")
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE task_queue SET run_id = ?
		 WHERE tenant_id = ? AND id = ? AND status = ? AND (run_id = '' OR run_id = ?)`,
		runID, normalizeTenant(tenantID), id, QueueStatusStarted, runID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return fmt.Errorf("queue row is not started or is already bound to another run")
	}
	return nil
}

// BindQueuedRunClaimed binds a run only for the worker that owns the current
// claim epoch. Legacy recovery/tests may keep using BindQueuedRun.
func (s *Store) BindQueuedRunClaimed(ctx context.Context, tenantID, id, runID, token string) error {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(runID) == "" || strings.TrimSpace(token) == "" {
		return fmt.Errorf("queue id, run id, and claim token are required")
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE task_queue SET run_id = ?
		 WHERE tenant_id = ? AND id = ? AND status = ? AND claim_token = ? AND (run_id = '' OR run_id = ?)`,
		runID, normalizeTenant(tenantID), id, QueueStatusStarted, token, runID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return fmt.Errorf("queue claim is stale or already bound to another run")
	}
	return nil
}

// RequeueDoneSystemQueuedIfUnmaterialized reopens a queue-level done row only
// when its bound run has no durable terminal event. This closes the crash
// window without ever replaying a successfully completed side effect.
func (s *Store) RequeueDoneSystemQueuedIfUnmaterialized(ctx context.Context, tenantID, id string, maxRestarts int) (bool, error) {
	if strings.TrimSpace(id) == "" {
		return false, fmt.Errorf("queue id is required")
	}
	if maxRestarts < 1 {
		maxRestarts = 1
	}
	tenant := normalizeTenant(tenantID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var runID string
	var restarts int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(run_id, ''), restarts FROM task_queue
		 WHERE tenant_id = ? AND id = ? AND idempotency_key != '' AND status = ?`,
		tenant, id, QueueStatusDone).Scan(&runID, &restarts); err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return false, nil
		}
		return false, err
	}
	if runID == "" || restarts >= maxRestarts {
		return false, nil
	}
	var terminalCount int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM task_events WHERE run_id = ? AND type = 'run.finished'`, runID).Scan(&terminalCount); err != nil {
		return false, err
	}
	if terminalCount > 0 {
		return false, nil
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE task_queue SET status = ?, run_id = '', restarts = restarts + 1,
			claim_token = '', lease_until = 0
		 WHERE tenant_id = ? AND id = ? AND status = ? AND run_id = ? AND restarts = ?`,
		QueueStatusQueued, tenant, id, QueueStatusDone, runID, restarts)
	if err != nil {
		return false, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return false, nil
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// RunHasSuccessfulTerminalEvent reports whether a run completed successfully.
// Interrupted, cancelled, and failed events remain recoverable evidence and
// must not suppress bounded queue compensation.
func (s *Store) RunHasSuccessfulTerminalEvent(ctx context.Context, runID string) (bool, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return false, nil
	}
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM task_events WHERE run_id = ? AND type = 'run.finished'`, runID).Scan(&count)
	return count > 0, err
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
		`UPDATE task_queue SET status = ?, claim_token = '', lease_until = 0
		 WHERE tenant_id = ? AND id = ?`,
		normalizeName(status, QueueStatusQueued), normalizeTenant(tenantID), id)
	return err
}

// MarkQueuedIfStatus performs a compare-and-swap transition. Run shutdown and
// completion race with one another, so unconditional writes can turn a row
// explicitly reopened for restart back into done while the old goroutine is
// unwinding.
func (s *Store) MarkQueuedIfStatus(ctx context.Context, tenantID, id, fromStatus, toStatus string) (bool, error) {
	if strings.TrimSpace(id) == "" {
		return false, fmt.Errorf("queue id is required")
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE task_queue SET status = ?, claim_token = '', lease_until = 0
		 WHERE tenant_id = ? AND id = ? AND status = ?`,
		normalizeName(toStatus, QueueStatusQueued), normalizeTenant(tenantID), id,
		normalizeName(fromStatus, QueueStatusQueued))
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n == 1, nil
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

// ListAllQueued returns due rows across all persons/tenants in scheduler order.
// The boot drain uses it to find every person with runnable work after restart.
func (s *Store) ListAllQueued(ctx context.Context, status string) ([]QueuedTask, error) {
	status = normalizeName(status, QueueStatusQueued)
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+queueSelectColumns+`
		 FROM task_queue WHERE status = ? AND COALESCE(not_before, 0) <= ?
		 ORDER BY priority DESC, created_at ASC, rowid ASC`,
		status, time.Now().Unix())
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// Finalization commits the terminal event before the queue defer marks its
	// row done. A crash in that narrow window must settle the row, not replay it.
	if _, err := tx.ExecContext(ctx,
		`UPDATE task_queue SET status = ?
		 WHERE status = ? AND COALESCE(run_id, '') != ''
		   AND EXISTS (
		       SELECT 1 FROM task_events e
		       WHERE e.run_id = task_queue.run_id AND e.type = 'run.finished'
		   )
		   AND (
		       idempotency_key NOT LIKE 'external-watch:%:finalization'
		       OR EXISTS (
		           SELECT 1 FROM effect_receipts r
		           WHERE r.tenant_id = task_queue.tenant_id
		             AND r.effect_key = task_queue.idempotency_key
		             AND r.delivery_enqueued = 1
		       )
		   )`,
		QueueStatusDone, QueueStatusStarted); err != nil {
		return 0, 0, err
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE task_queue SET status = ?, run_id = '', restarts = restarts + 1,
			claim_token = '', lease_until = 0
		 WHERE status = ? AND restarts < ?`,
		QueueStatusQueued, QueueStatusStarted, maxQueueRestarts)
	if err != nil {
		return 0, 0, err
	}
	nRequeued, _ := res.RowsAffected()
	res, err = tx.ExecContext(ctx,
		`UPDATE task_queue SET status = ? WHERE status = ?`,
		QueueStatusFailed,
		QueueStatusStarted)
	if err != nil {
		return int(nRequeued), 0, err
	}
	nDropped, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return int(nRequeued), int(nDropped), nil
}
