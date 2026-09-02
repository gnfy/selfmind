package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"selfmind/internal/executionenv"
)

// Persistent steering mailbox (Loop Engineering ACTIVE PLAN P0-A). Mid-run
// guidance used to live only in the run's in-memory channel plus a 120-char
// preview event, so a daemon crash in any window between HTTP Accepted and
// the agent's drain silently lost cross-endpoint instructions. The mailbox is
// the durable record; the in-memory channel is merely delivery. Lifecycle:
//
//	accepted  -> claimed   (handed to the live run's channel)
//	          -> consumed  (the kernel folded it into a model step —
//	                        marked when run.steering_consumed commits)
//	          -> deferred  (run ended or daemon restarted before consumption;
//	                        re-queued as durable task-pinned work)
//	          -> expired   (back-pressure rejection or stale beyond the replay
//	                        window; never replayed)
const (
	SteeringAccepted = "accepted"
	SteeringClaimed  = "claimed"
	SteeringConsumed = "consumed"
	SteeringDeferred = "deferred"
	SteeringExpired  = "expired"
)

type SteeringMessage struct {
	ID             string
	TenantID       string
	PersonID       string
	RunID          string
	TaskID         string
	Channel        string
	Platform       string
	PlatformUserID string
	WorkspaceID    string
	ApprovalMode   string
	Content        string
	ContentHash    string
	Status         string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// SteeringContentHash is the deterministic consumption-matching key between a
// mailbox row and the kernel's steering-consumed event.
func SteeringContentHash(content string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(content)))
	return hex.EncodeToString(sum[:16])
}

// AcceptSteering persists guidance BEFORE the caller reports Accepted. The
// row is the durability contract: if this insert fails the endpoint must
// fail, never pretend acceptance.
func (s *Store) AcceptSteering(ctx context.Context, m SteeringMessage) (*SteeringMessage, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("control store is unavailable")
	}
	m.TenantID = normalizeTenant(m.TenantID)
	m.PersonID = strings.TrimSpace(m.PersonID)
	m.Content = strings.TrimSpace(m.Content)
	if m.PersonID == "" || m.Content == "" {
		return nil, fmt.Errorf("person id and content are required")
	}
	m.ID = "steer_" + uuid.NewString()
	m.ContentHash = SteeringContentHash(m.Content)
	m.Status = SteeringAccepted
	now := time.Now()
	m.CreatedAt = now
	m.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `INSERT INTO steering_mailbox
		(id, tenant_id, person_id, run_id, task_id, channel, platform, platform_user_id,
		 workspace_id, approval_mode, content, content_hash, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.TenantID, m.PersonID, m.RunID, m.TaskID, m.Channel, m.Platform, m.PlatformUserID,
		m.WorkspaceID, m.ApprovalMode,
		m.Content, m.ContentHash, m.Status, now.Unix(), now.Unix())
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// MarkSteeringClaimed records the successful hand-off into the live run's
// in-memory channel. CAS from accepted so replays cannot regress state.
func (s *Store) MarkSteeringClaimed(ctx context.Context, tenantID, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE steering_mailbox
		SET status = ?, updated_at = ? WHERE tenant_id = ? AND id = ? AND status = ?`,
		SteeringClaimed, time.Now().Unix(), normalizeTenant(tenantID), id, SteeringAccepted)
	return err
}

// MarkSteeringExpired terminates a row that must never replay (back-pressure
// rejection, or staleness at boot). Reason is audit only.
func (s *Store) MarkSteeringExpired(ctx context.Context, tenantID, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE steering_mailbox
		SET status = ?, updated_at = ? WHERE tenant_id = ? AND id = ? AND status IN (?, ?)`,
		SteeringExpired, time.Now().Unix(), normalizeTenant(tenantID), id,
		SteeringAccepted, SteeringClaimed)
	return err
}

// ConsumeSteeringByHash marks the OLDEST live row matching (run, content
// hash) consumed. Called when the gateway commits the kernel's
// run.steering_consumed event — the only proof the guidance reached a model
// step. Returns whether a row was consumed (false = guidance predating the
// mailbox, or already consumed).
func (s *Store) ConsumeSteeringByHash(ctx context.Context, tenantID, runID, contentHash string) (bool, error) {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(contentHash) == "" {
		return false, nil
	}
	result, err := s.db.ExecContext(ctx, `UPDATE steering_mailbox
		SET status = ?, updated_at = ?
		WHERE id = (SELECT id FROM steering_mailbox
			WHERE tenant_id = ? AND run_id = ? AND content_hash = ? AND status IN (?, ?)
			ORDER BY created_at ASC LIMIT 1)`,
		SteeringConsumed, time.Now().Unix(),
		normalizeTenant(tenantID), runID, contentHash, SteeringAccepted, SteeringClaimed)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n == 1, nil
}

// ConsumeSteeringByID marks one exact mailbox row consumed. Content hashes
// are not unique, so new kernel events always use this method; the hash-based
// method remains only for events written by older binaries.
func (s *Store) ConsumeSteeringByID(ctx context.Context, tenantID, runID, id string) (bool, error) {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(id) == "" {
		return false, nil
	}
	result, err := s.db.ExecContext(ctx, `UPDATE steering_mailbox
		SET status = ?, updated_at = ?
		WHERE tenant_id = ? AND run_id = ? AND id = ? AND status IN (?, ?)`,
		SteeringConsumed, time.Now().Unix(), normalizeTenant(tenantID), runID, id,
		SteeringAccepted, SteeringClaimed)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n == 1, nil
}

// ListUnconsumedSteering returns live (accepted/claimed) rows, optionally
// scoped to one run, oldest first. Run finalization and boot recovery use it
// to defer leftovers into the durable queue.
func (s *Store) ListUnconsumedSteering(ctx context.Context, tenantID, runID string, limit int) ([]SteeringMessage, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := `SELECT id, tenant_id, person_id, COALESCE(run_id, ''), COALESCE(task_id, ''),
		COALESCE(channel, ''), COALESCE(platform, ''), COALESCE(platform_user_id, ''),
		COALESCE(workspace_id, ''), COALESCE(approval_mode, ''),
		content, content_hash, status, created_at, updated_at
		FROM steering_mailbox WHERE status IN (?, ?)`
	args := []interface{}{SteeringAccepted, SteeringClaimed}
	if strings.TrimSpace(tenantID) != "" {
		query += ` AND tenant_id = ?`
		args = append(args, normalizeTenant(tenantID))
	}
	if strings.TrimSpace(runID) != "" {
		query += ` AND run_id = ?`
		args = append(args, runID)
	}
	query += ` ORDER BY created_at ASC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SteeringMessage
	for rows.Next() {
		m, err := scanSteering(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeferSteering re-homes an unconsumed row into the durable task queue so the
// guidance survives run completion or a daemon restart as ordinary next-turn
// input. It is deliberately not pinned to the finished task: Main never saw
// this input, so no component may pre-decide whether it was related. The
// queue's idempotency key (steering:<id>) makes
// crash-replay of this hand-off converge on one row; the mailbox row flips to
// deferred only after the enqueue succeeded.
func (s *Store) DeferSteering(ctx context.Context, m SteeringMessage) error {
	var executionRoots []executionenv.RootBinding
	if run, err := s.GetRun(ctx, m.TenantID, m.RunID); err != nil {
		return err
	} else if run != nil {
		executionRoots = executionenv.CloneRootBindings(run.ExecutionRoots)
	}
	if _, err := s.EnqueueQueued(ctx, QueuedTask{
		TenantID:       m.TenantID,
		PersonID:       m.PersonID,
		Channel:        m.Channel,
		Platform:       m.Platform,
		PlatformUserID: m.PlatformUserID,
		Content:        m.Content,
		ApprovalMode:   m.ApprovalMode,
		WorkspaceID:    m.WorkspaceID,
		ExecutionRoots: executionRoots,
		IdempotencyKey: "steering:" + m.ID,
		Class:          QueueClassForeground,
	}); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE steering_mailbox
		SET status = ?, updated_at = ? WHERE tenant_id = ? AND id = ? AND status IN (?, ?)`,
		SteeringDeferred, time.Now().Unix(), normalizeTenant(m.TenantID), m.ID,
		SteeringAccepted, SteeringClaimed)
	return err
}

// QueueSteeringAsIndependent moves one exact Main-consumed live input into a
// fresh foreground queue item. It intentionally drops TaskID/ReplyToRunID: Main
// has classified this input as independent of the active objective, so the
// drained turn must resolve a new root instead of inheriting active ownership.
// The stable queue key makes a provider retry or crash replay converge on one
// row. accepted/claimed are allowed because the event consumer that records
// run.steering_consumed may lag the model tool call by a few milliseconds.
func (s *Store) QueueSteeringAsIndependent(ctx context.Context, tenantID, personID, runID, steeringID string) (*QueuedTask, error) {
	return s.queueSteeringAsWork(ctx, tenantID, personID, runID, steeringID, "")
}

// QueueSteeringAsContinuation moves one exact Main-consumed input into the
// queue with a validated historical parent. The parent domain, not the active
// Run or source endpoint, owns the eventual child's task/workspace/roots.
func (s *Store) QueueSteeringAsContinuation(ctx context.Context, tenantID, personID, runID, steeringID, parentRunID string) (*QueuedTask, error) {
	if strings.TrimSpace(parentRunID) == "" {
		return nil, fmt.Errorf("parent run id is required")
	}
	return s.queueSteeringAsWork(ctx, tenantID, personID, runID, steeringID, parentRunID)
}

func (s *Store) queueSteeringAsWork(ctx context.Context, tenantID, personID, runID, steeringID, parentRunID string) (*QueuedTask, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("control store is unavailable")
	}
	tenantID = normalizeTenant(tenantID)
	personID = strings.TrimSpace(personID)
	runID = strings.TrimSpace(runID)
	steeringID = strings.TrimSpace(steeringID)
	if personID == "" || runID == "" || steeringID == "" {
		return nil, fmt.Errorf("person id, run id, and input id are required")
	}
	row := s.db.QueryRowContext(ctx, `SELECT id, tenant_id, person_id, COALESCE(run_id, ''), COALESCE(task_id, ''),
		COALESCE(channel, ''), COALESCE(platform, ''), COALESCE(platform_user_id, ''),
		COALESCE(workspace_id, ''), COALESCE(approval_mode, ''),
		content, content_hash, status, created_at, updated_at
		FROM steering_mailbox WHERE tenant_id = ? AND id = ?`, tenantID, steeringID)
	m, err := scanSteering(row)
	if err != nil {
		return nil, fmt.Errorf("load live input: %w", err)
	}
	if m.PersonID != personID || m.RunID != runID {
		return nil, fmt.Errorf("live input does not belong to this run")
	}
	queueKey := "steering-independent:" + m.ID
	if strings.TrimSpace(parentRunID) != "" {
		queueKey = "steering-continuation:" + m.ID + ":" + strings.TrimSpace(parentRunID)
	}
	if m.Status == SteeringDeferred {
		// A retry of this exact tool call returns the row it already created.
		// A generic steering:<id> finalization/boot deferral is different: Main
		// never classified that input, so creating another independent row would
		// duplicate user work.
		queued, err := s.GetQueuedByIdempotencyKey(ctx, tenantID, queueKey)
		if err != nil {
			return nil, err
		}
		if queued != nil {
			return queued, nil
		}
		return nil, fmt.Errorf("live input was already transferred as next-turn work")
	}
	switch m.Status {
	case SteeringAccepted, SteeringClaimed, SteeringConsumed:
	default:
		return nil, fmt.Errorf("live input is no longer queueable")
	}
	var executionRoots []executionenv.RootBinding
	workspaceID := m.WorkspaceID
	taskID := ""
	replyToRunID := ""
	if active, err := s.GetRun(ctx, tenantID, runID); err != nil {
		return nil, err
	} else if active == nil || active.PersonID != personID {
		return nil, fmt.Errorf("active run is no longer available")
	} else {
		executionRoots = executionenv.CloneRootBindings(active.ExecutionRoots)
	}
	if strings.TrimSpace(parentRunID) != "" {
		parent, err := s.GetRun(ctx, tenantID, parentRunID)
		if err != nil {
			return nil, err
		}
		if parent == nil || parent.PersonID != personID {
			return nil, fmt.Errorf("continuation parent is unavailable for the current person")
		}
		resumable, err := s.ListUnresolvedRuns(ctx, tenantID, personID, parent.TaskID, 20)
		if err != nil {
			return nil, err
		}
		found := false
		for _, candidate := range resumable {
			if candidate.ID == parent.ID {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("continuation parent is no longer resumable")
		}
		taskID = parent.TaskID
		replyToRunID = parent.ID
		workspaceID = parent.WorkspaceID
		executionRoots = executionenv.CloneRootBindings(parent.ExecutionRoots)
	}
	queued, err := s.EnqueueQueued(ctx, QueuedTask{
		TenantID:       tenantID,
		PersonID:       personID,
		Channel:        m.Channel,
		Platform:       m.Platform,
		PlatformUserID: m.PlatformUserID,
		Content:        m.Content,
		ApprovalMode:   m.ApprovalMode,
		WorkspaceID:    workspaceID,
		ExecutionRoots: executionRoots,
		TaskID:         taskID,
		ReplyToRunID:   replyToRunID,
		IdempotencyKey: queueKey,
		Class:          QueueClassForeground,
	})
	if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE steering_mailbox
		SET status = ?, updated_at = ?
		WHERE tenant_id = ? AND id = ? AND person_id = ? AND run_id = ?
		  AND status IN (?, ?, ?)`,
		SteeringDeferred, time.Now().Unix(), tenantID, m.ID, personID, runID,
		SteeringAccepted, SteeringClaimed, SteeringConsumed); err != nil {
		return nil, err
	}
	return queued, nil
}

// RecoverSteeringAtBoot defers every live row whose run died with the previous
// daemon. Accepted guidance has no age-based expiry: once acknowledged it is
// durable user intent until consumed, explicitly rejected by back-pressure,
// or re-homed as queued work.
func (s *Store) RecoverSteeringAtBoot(ctx context.Context) (int, int, error) {
	deferred, expired := 0, 0
	for {
		pending, err := s.ListUnconsumedSteering(ctx, "", "", 200)
		if err != nil {
			return deferred, expired, err
		}
		if len(pending) == 0 {
			return deferred, expired, nil
		}
		for _, m := range pending {
			if err := s.DeferSteering(ctx, m); err != nil {
				return deferred, expired, err
			}
			deferred++
		}
	}
}

func scanSteering(rows interface{ Scan(dest ...any) error }) (SteeringMessage, error) {
	var m SteeringMessage
	var created, updated int64
	if err := rows.Scan(&m.ID, &m.TenantID, &m.PersonID, &m.RunID, &m.TaskID,
		&m.Channel, &m.Platform, &m.PlatformUserID, &m.WorkspaceID, &m.ApprovalMode,
		&m.Content, &m.ContentHash, &m.Status, &created, &updated); err != nil {
		return SteeringMessage{}, err
	}
	m.CreatedAt = time.Unix(created, 0)
	m.UpdatedAt = time.Unix(updated, 0)
	return m, nil
}
