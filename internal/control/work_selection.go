package control

import (
	"context"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/executionenv"
)

// EnqueueSelectedContinuation commits a validated Main proposal as a durable
// exact-parent turn. The caller supplies the original source request and route;
// this method overwrites every ownership/execution field from the parent Run.
func (s *Store) EnqueueSelectedContinuation(ctx context.Context, tenantID, personID, sourceRunID, parentRunID string, q QueuedTask) (*QueuedTask, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("control store is unavailable")
	}
	tenantID = normalizeTenant(tenantID)
	personID = strings.TrimSpace(personID)
	sourceRunID = strings.TrimSpace(sourceRunID)
	parentRunID = strings.TrimSpace(parentRunID)
	if personID == "" || sourceRunID == "" || parentRunID == "" || strings.TrimSpace(q.Content) == "" {
		return nil, fmt.Errorf("person, source run, parent run, and content are required")
	}
	source, err := s.GetRun(ctx, tenantID, sourceRunID)
	if err != nil {
		return nil, err
	}
	if source == nil || source.PersonID != personID {
		return nil, fmt.Errorf("source run is unavailable for the current person")
	}
	parent, err := s.GetRun(ctx, tenantID, parentRunID)
	if err != nil {
		return nil, err
	}
	if parent == nil || parent.PersonID != personID {
		return nil, fmt.Errorf("continuation parent is unavailable for the current person")
	}
	candidates, err := s.ListUnresolvedRuns(ctx, tenantID, personID, parent.TaskID, 20)
	if err != nil {
		return nil, err
	}
	resumable := false
	for _, candidate := range candidates {
		if candidate.ID == parent.ID {
			resumable = true
			break
		}
	}
	if !resumable {
		return nil, fmt.Errorf("continuation parent is no longer resumable")
	}
	q.TenantID = tenantID
	q.PersonID = personID
	q.TaskID = parent.TaskID
	q.ReplyToRunID = parent.ID
	q.WorkspaceID = parent.WorkspaceID
	q.ExecutionRoots = executionenv.CloneRootBindings(parent.ExecutionRoots)
	q.IdempotencyKey = "work-selection:" + sourceRunID + ":" + parentRunID
	q.Class = QueueClassForeground
	return s.EnqueueQueued(ctx, q)
}

// RunSelectionEffectBoundary reports whether an implicit historical selection
// arrived after the current interaction produced material state. Read-only
// discovery and lifecycle/broker bookkeeping are safe; any other tool effect,
// approval, clarification, watch, artifact, handoff, or outbound delivery
// closes the automatic transfer window.
func (s *Store) RunSelectionEffectBoundary(ctx context.Context, tenantID, personID, runID string) (bool, string, error) {
	if s == nil || s.db == nil {
		return false, "", fmt.Errorf("control store is unavailable")
	}
	tenantID = normalizeTenant(tenantID)
	personID = strings.TrimSpace(personID)
	runID = strings.TrimSpace(runID)
	run, err := s.GetRun(ctx, tenantID, runID)
	if err != nil {
		return false, "", err
	}
	if run == nil || run.PersonID != personID {
		return false, "", fmt.Errorf("run is unavailable for the current person")
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tool_ledger
		WHERE tenant_id = ? AND run_id = ? AND retry_class <> 'read_only'
		  AND tool_name NOT IN ('work_select', 'update_plan', 'finish_run')`, tenantID, runID).Scan(&count); err != nil {
		return false, "", err
	}
	if count > 0 {
		return true, "tool_effect", nil
	}
	checks := []struct {
		reason string
		query  string
	}{
		{"approval", `SELECT COUNT(*) FROM approval_requests WHERE tenant_id = ? AND person_id = ? AND run_id = ?`},
		{"clarification", `SELECT COUNT(*) FROM clarify_requests WHERE tenant_id = ? AND person_id = ? AND run_id = ?`},
		{"external_watch", `SELECT COUNT(*) FROM external_watches WHERE tenant_id = ? AND person_id = ? AND run_id = ?`},
		{"artifact", `SELECT COUNT(*) FROM task_artifacts a
			JOIN runs r ON r.id = a.run_id AND r.thread_id = a.thread_id
			WHERE r.tenant_id = ? AND r.person_id = ? AND r.id = ?`},
		{"handoff", `SELECT COUNT(*) FROM task_handoffs h
			JOIN runs r ON r.id = h.run_id AND r.thread_id = h.thread_id
			WHERE r.tenant_id = ? AND r.person_id = ? AND r.id = ?`},
		{"delivery", `SELECT COUNT(*) FROM outbound_messages WHERE tenant_id = ? AND person_id = ? AND run_id = ?`},
	}
	for _, check := range checks {
		args := []interface{}{tenantID, personID, runID}
		if err := s.db.QueryRowContext(ctx, check.query, args...).Scan(&count); err != nil {
			return false, "", err
		}
		if count > 0 {
			return true, check.reason, nil
		}
	}
	return false, "", nil
}

// ProjectInteractionTask keeps an OBSERVE turn auditable while removing its
// synthetic one-run label from ordinary task views. It fails closed if the
// task has accumulated any other Run, because such a label may be user-owned.
// Automation only moves visibility upward: a pinned Thread, a Run that did
// work, or a Thread listed by promotion evidence keeps its place, and only a
// tool-free interaction folds back into unlisted history.
func (s *Store) ProjectInteractionTask(ctx context.Context, tenantID, personID, taskID, runID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("control store is unavailable")
	}
	tenantID = normalizeTenant(tenantID)
	personID = strings.TrimSpace(personID)
	taskID = strings.TrimSpace(taskID)
	runID = strings.TrimSpace(runID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs
		WHERE tenant_id = ? AND person_id = ? AND thread_id = ?`, tenantID, personID, taskID).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("interaction projection requires a single-run task")
	}
	var exact int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs
		WHERE tenant_id = ? AND person_id = ? AND thread_id = ? AND id = ?`, tenantID, personID, taskID, runID).Scan(&exact); err != nil {
		return err
	}
	if exact != 1 {
		return fmt.Errorf("interaction run does not belong to the task")
	}
	var pinned int
	var visibility, status string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(t.pinned, 0), COALESCE(t.visibility, 'listed'), r.status
		FROM threads t JOIN runs r ON r.tenant_id = t.tenant_id AND r.thread_id = t.id
		WHERE t.tenant_id = ? AND t.person_id = ? AND t.id = ? AND r.id = ?`,
		tenantID, personID, taskID, runID).Scan(&pinned, &visibility, &status); err != nil {
		return fmt.Errorf("interaction task not found: %w", err)
	}
	if pinned != 0 {
		return nil
	}
	if evidence, err := runHasWorkEvidenceTx(ctx, tx, tenantID, runID); err != nil {
		return err
	} else if evidence {
		return nil
	}
	if normalizeTaskVisibility(visibility) == TaskVisibilityListed && runStatusPromotesThread(status) {
		return nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE threads SET kind = 'interaction', visibility = 'unlisted', updated_at = ?
		WHERE tenant_id = ? AND person_id = ? AND id = ?`, time.Now().Unix(), tenantID, personID, taskID)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return fmt.Errorf("interaction task not found")
	}
	return tx.Commit()
}
