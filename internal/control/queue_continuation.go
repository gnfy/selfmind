package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"selfmind/internal/executionenv"
)

// ResolveQueuedContinuation follows the one-child Run lineage of an exact
// queued reply. A recovery or watcher may claim its original parent while the
// reply waits. The reply remains on the same person's work and waits for a
// running child; once that child settles it can claim the newest unresolved
// descendant, or start a task-pinned turn if the work is already complete.
// It does not change the durable row: every claim attempt rechecks the latest
// lineage, including after a daemon restart.
func (s *Store) ResolveQueuedContinuation(ctx context.Context, q QueuedTask) (resolved QueuedTask, wait bool, err error) {
	if strings.TrimSpace(q.ReplyToRunID) == "" {
		return q, false, nil
	}
	if s == nil || s.db == nil || q.PersonID == "" {
		return q, false, fmt.Errorf("exact queued continuation has no owner")
	}
	tenantID := normalizeTenant(q.TenantID)
	current := strings.TrimSpace(q.ReplyToRunID)
	// System finalization and recovery rows describe one exact effect boundary.
	// Rebasing them onto a later child could re-run a dispatch or verification.
	// Only person-authored replies may follow a claimed lineage.
	systemExact := q.Class == QueueClassFinalization || q.Class == QueueClassRecovery ||
		strings.HasPrefix(q.IdempotencyKey, "external-watch:") ||
		strings.HasPrefix(q.IdempotencyKey, "run-recovery:") ||
		strings.HasPrefix(q.IdempotencyKey, "approval-resume:")
	for depth := 0; depth < 64; depth++ {
		var personID, taskID, status, legacyChild, workspaceID, rootsJSON string
		err := s.db.QueryRowContext(ctx, `SELECT person_id, thread_id, status, COALESCE(resumed_by_run_id, ''),
			COALESCE(workspace_id, ''), COALESCE(execution_roots_json, '[]')
			FROM runs WHERE tenant_id = ? AND id = ?`, tenantID, current).
			Scan(&personID, &taskID, &status, &legacyChild, &workspaceID, &rootsJSON)
		if errors.Is(err, sql.ErrNoRows) {
			return q, false, fmt.Errorf("queued continuation parent %s is missing", current)
		}
		if err != nil {
			return q, false, err
		}
		if personID != q.PersonID || (q.TaskID != "" && taskID != q.TaskID) {
			return q, false, fmt.Errorf("queued continuation parent %s changed owner or task", current)
		}
		q.TaskID, q.WorkspaceID = taskID, workspaceID
		var roots []executionenv.RootBinding
		if err := json.Unmarshal([]byte(rootsJSON), &roots); err != nil {
			return q, false, err
		}
		q.ExecutionRoots = roots
		var childID string
		err = s.db.QueryRowContext(ctx, `SELECT id FROM runs WHERE tenant_id = ? AND resumes_run_id = ?`, tenantID, current).Scan(&childID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return q, false, err
		}
		if childID == "" {
			childID = legacyChild
		}
		if childID != "" {
			if systemExact {
				return q, false, nil
			}
			if childID == current {
				return q, false, fmt.Errorf("queued continuation cycle at %s", current)
			}
			current = childID
			continue
		}
		switch status {
		case "running":
			return q, true, nil
		case "waiting_external":
			// A watcher finalization is the exact queued child of this Run.
			// It becomes claimable only after every watch has concluded, using
			// the same condition as validateResumeClaimTx.
			var live int
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM external_watches
				WHERE tenant_id = ? AND run_id = ? AND status IN ('pending', 'running')`, tenantID, current).Scan(&live); err != nil {
				return q, false, err
			}
			if live > 0 {
				return q, true, nil
			}
			pending, err := s.IsRunExternalResourceWaitPending(ctx, tenantID, current)
			if err != nil {
				return q, false, err
			}
			if pending {
				return q, true, nil
			}
			q.ReplyToRunID = current
			return q, false, nil
		case "interrupted", "waiting_user", "verification_partial", "blocked":
			q.ReplyToRunID = current
			return q, false, nil
		default:
			if systemExact {
				return q, false, nil
			}
			q.ReplyToRunID = ""
			return q, false, nil
		}
	}
	return q, false, fmt.Errorf("queued continuation lineage is too deep")
}
