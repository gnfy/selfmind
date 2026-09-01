package control

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// ContinuityAuditFinding is one read-only observation from the historical
// Task/Run continuity audit (simplification §10.4). The audit never moves
// runs, rewrites edges, touches memory, or deletes anything; the only
// optional repair is reconciling a task's DERIVED status projection, and even
// that runs only under an explicit --apply.
type ContinuityAuditFinding struct {
	Kind    string // legacy_resume_unfilled | legacy_resume_conflict | illegal_parent_edge | ownerless_approval | ownerless_clarify | projection_mismatch | stale_wait_projection
	TaskID  string
	RunID   string
	Detail  string
	SafeFix bool // true only for projection_mismatch (reducer-derived)
}

// AuditTaskRunContinuity runs the §10.4 read-only checks:
//   - legacy resumed_by edges that the v7 backfill could not convert;
//   - legacy resumed_by values claiming more than one parent;
//   - parent edges disagreeing with their child on tenant/person/task;
//   - pending approvals/clarifies whose task or origin run is missing;
//   - task status projections that disagree with the derived reduction.
func (s *Store) AuditTaskRunContinuity(ctx context.Context, tenantID string, limit int) ([]ContinuityAuditFinding, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("control store is unavailable")
	}
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	tenant := normalizeTenant(tenantID)
	var findings []ContinuityAuditFinding
	add := func(kind, taskID, runID, detail string, safe bool) {
		if len(findings) < limit {
			findings = append(findings, ContinuityAuditFinding{Kind: kind, TaskID: taskID, RunID: runID, Detail: detail, SafeFix: safe})
		}
	}

	// Legacy reverse edges the forward backfill did not convert.
	rows, err := s.db.QueryContext(ctx, `SELECT p.id, p.task_id, p.resumed_by_run_id,
		COALESCE((SELECT c.parent_run_id FROM task_runs c WHERE c.tenant_id = p.tenant_id AND c.id = p.resumed_by_run_id), '')
		FROM task_runs p
		WHERE p.tenant_id = ? AND COALESCE(p.resumed_by_run_id, '') <> ''`, tenant)
	if err != nil {
		return nil, err
	}
	claimCounts := map[string][]string{}
	for rows.Next() {
		var parentID, taskID, childID, childParent string
		if err := rows.Scan(&parentID, &taskID, &childID, &childParent); err != nil {
			rows.Close()
			return nil, err
		}
		claimCounts[childID] = append(claimCounts[childID], parentID)
		if childParent != parentID {
			add("legacy_resume_unfilled", taskID, parentID,
				fmt.Sprintf("legacy resumed_by -> %s has no matching forward parent edge", childID), false)
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for childID, parents := range claimCounts {
		if len(parents) > 1 {
			add("legacy_resume_conflict", "", childID,
				fmt.Sprintf("child claimed by %d legacy parents: %s", len(parents), strings.Join(parents, ", ")), false)
		}
	}

	// Forward parent edges must agree with their child on tenant/person/task.
	rows, err = s.db.QueryContext(ctx, `SELECT c.id, c.task_id, c.parent_run_id,
		COALESCE(p.task_id, ''), COALESCE(p.person_id, ''), c.person_id
		FROM task_runs c
		LEFT JOIN task_runs p ON p.tenant_id = c.tenant_id AND p.id = c.parent_run_id
		WHERE c.tenant_id = ? AND COALESCE(c.parent_run_id, '') <> ''`, tenant)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var childID, childTask, parentID, parentTask, parentPerson, childPerson string
		if err := rows.Scan(&childID, &childTask, &parentID, &parentTask, &parentPerson, &childPerson); err != nil {
			rows.Close()
			return nil, err
		}
		switch {
		case parentTask == "":
			add("illegal_parent_edge", childTask, childID, fmt.Sprintf("parent %s does not exist", parentID), false)
		case parentTask != childTask:
			add("illegal_parent_edge", childTask, childID, fmt.Sprintf("parent %s belongs to task %s", parentID, parentTask), false)
		case parentPerson != childPerson:
			add("illegal_parent_edge", childTask, childID, fmt.Sprintf("parent %s belongs to another person", parentID), false)
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	// Pending human input whose owner rows are gone.
	for _, probe := range []struct{ kind, query string }{
		{"ownerless_approval", `SELECT a.id, COALESCE(a.task_id, ''), COALESCE(a.run_id, '') FROM approval_requests a
			WHERE a.tenant_id = ? AND a.status = 'pending'
			  AND (COALESCE(a.task_id, '') = '' OR NOT EXISTS (SELECT 1 FROM tasks t WHERE t.tenant_id = a.tenant_id AND t.id = a.task_id)
			       OR (COALESCE(a.run_id, '') <> '' AND NOT EXISTS (SELECT 1 FROM task_runs r WHERE r.tenant_id = a.tenant_id AND r.id = a.run_id)))`},
		{"ownerless_clarify", `SELECT c.id, COALESCE(c.task_id, ''), COALESCE(c.run_id, '') FROM clarify_requests c
			WHERE c.tenant_id = ? AND c.status = 'pending'
			  AND (COALESCE(c.task_id, '') = '' OR NOT EXISTS (SELECT 1 FROM tasks t WHERE t.tenant_id = c.tenant_id AND t.id = c.task_id)
			       OR (COALESCE(c.run_id, '') <> '' AND NOT EXISTS (SELECT 1 FROM task_runs r WHERE r.tenant_id = c.tenant_id AND r.id = c.run_id)))`},
	} {
		rows, err = s.db.QueryContext(ctx, probe.query, tenant)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, taskID, runID string
			if err := rows.Scan(&id, &taskID, &runID); err != nil {
				rows.Close()
				return nil, err
			}
			add(probe.kind, taskID, runID, fmt.Sprintf("pending row %s has no live owner", id), false)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}

	// Task projection vs the derived reduction (read-only comparison).
	rows, err = s.db.QueryContext(ctx, `SELECT id, status FROM tasks
		WHERE tenant_id = ? AND archived_at IS NULL AND status NOT IN ('archived', 'cancelled')
		ORDER BY updated_at DESC LIMIT ?`, tenant, limit)
	if err != nil {
		return nil, err
	}
	type projected struct{ id, status string }
	var tasks []projected
	for rows.Next() {
		var item projected
		if err := rows.Scan(&item.id, &item.status); err != nil {
			rows.Close()
			return nil, err
		}
		tasks = append(tasks, item)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, task := range tasks {
		derived, err := s.derivedWaitStatus(ctx, tenant, task.id)
		if err != nil {
			return nil, err
		}
		switch {
		case derived != "" && derived != task.status:
			// Live wait evidence disagrees with the projection. The reducer
			// repair lands exactly on this derived value because wait
			// evidence outranks any proposed status.
			add("projection_mismatch", task.id, "",
				fmt.Sprintf("stored status %q, derived %q", task.status, derived), true)
		case derived == "" && staleWaitProjectionStatuses[task.status]:
			// The projection claims a wait but no live evidence backs it.
			// The correct terminal label lives in the run outcome, so this
			// stays a human-review finding.
			add("stale_wait_projection", task.id, "",
				fmt.Sprintf("stored status %q has no live wait evidence", task.status), false)
		}
	}
	return findings, nil
}

// staleWaitProjectionStatuses are task projections that must be backed by
// live evidence (pending human input, an active watch, a queued watcher
// finalization, or an unclaimed resumable run). in_progress is deliberately
// absent: a direct answer legitimately leaves the label open with no run.
var staleWaitProjectionStatuses = map[string]bool{
	"waiting_user":         true,
	"waiting_external":     true,
	"waiting_finalization": true,
	"blocked":              true,
	"verification_partial": true,
	"interrupted":          true,
}

// derivedWaitStatus runs the finalization reducer read-only with an empty
// proposal: a non-empty result is a wait state demanded by live evidence,
// and an empty result means nothing currently holds the task.
func (s *Store) derivedWaitStatus(ctx context.Context, tenantID, taskID string) (string, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: false})
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	return resolveFinalTaskStatusTx(ctx, tx, normalizeTenant(tenantID), taskID, "", "")
}

// ReconcileTaskProjection re-derives one task's status projection through the
// production reducer. It is the ONLY repair the continuity audit may apply.
func (s *Store) ReconcileTaskProjection(ctx context.Context, tenantID, taskID string) error {
	current, err := s.GetTask(ctx, tenantID, taskID)
	if err != nil {
		return err
	}
	if current == nil {
		return fmt.Errorf("task not found: %s", taskID)
	}
	return s.ReconcileTaskAfterRun(ctx, tenantID, taskID, "", current.Status)
}
