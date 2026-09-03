package control

import (
	"context"
	"fmt"
	"strings"
)

// ContinuityAuditFinding is one read-only observation from the historical
// Task/Run continuity audit (simplification §10.4). The audit never moves
// runs, rewrites edges, touches memory, or deletes anything; the only
// no aggregate execution-state repair exists because Thread has no status.
type ContinuityAuditFinding struct {
	Kind    string // legacy_resume_unfilled | legacy_resume_conflict | illegal_parent_edge | ownerless_approval | ownerless_clarify
	TaskID  string
	RunID   string
	Detail  string
	SafeFix bool
}

// AuditTaskRunContinuity runs the §10.4 read-only checks:
//   - legacy resumed_by edges that the v7 backfill could not convert;
//   - legacy resumed_by values claiming more than one parent;
//   - parent edges disagreeing with their child on tenant/person/task;
//   - pending approvals/clarifies whose Thread or origin Run is missing.
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
	rows, err := s.db.QueryContext(ctx, `SELECT p.id, p.thread_id, p.resumed_by_run_id,
		COALESCE((SELECT c.parent_run_id FROM runs c WHERE c.tenant_id = p.tenant_id AND c.id = p.resumed_by_run_id), '')
		FROM runs p
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
	rows, err = s.db.QueryContext(ctx, `SELECT c.id, c.thread_id, c.parent_run_id,
		COALESCE(p.thread_id, ''), COALESCE(p.person_id, ''), c.person_id
		FROM runs c
		LEFT JOIN runs p ON p.tenant_id = c.tenant_id AND p.id = c.parent_run_id
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
		{"ownerless_approval", `SELECT a.id, COALESCE(a.thread_id, ''), COALESCE(a.run_id, '') FROM approval_requests a
			WHERE a.tenant_id = ? AND a.status = 'pending'
			  AND (COALESCE(a.thread_id, '') = '' OR NOT EXISTS (SELECT 1 FROM threads t WHERE t.tenant_id = a.tenant_id AND t.id = a.thread_id)
			       OR (COALESCE(a.run_id, '') <> '' AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.tenant_id = a.tenant_id AND r.id = a.run_id)))`},
		{"ownerless_clarify", `SELECT c.id, COALESCE(c.thread_id, ''), COALESCE(c.run_id, '') FROM clarify_requests c
			WHERE c.tenant_id = ? AND c.status = 'pending'
			  AND (COALESCE(c.thread_id, '') = '' OR NOT EXISTS (SELECT 1 FROM threads t WHERE t.tenant_id = c.tenant_id AND t.id = c.thread_id)
			       OR (COALESCE(c.run_id, '') <> '' AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.tenant_id = c.tenant_id AND r.id = c.run_id)))`},
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

	return findings, nil
}
