package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"selfmind/internal/executionenv"
)

var (
	ErrContinuationDomainMismatch = fmt.Errorf("continuation execution domain does not match the interaction run")
	ErrParentCheckpointRequired   = fmt.Errorf("continuation parent requires checkpoint reconstruction")
)

// ClaimInteractionContinuation turns a read-only interaction Run into the
// direct child of one exact historical Run when both already share the same
// frozen execution domain. Parent ownership, run/task re-pointing, dependent
// rows, blocker settlement, and placeholder cleanup commit together. A caller
// must use a transfer child instead when this reports a domain/checkpoint
// mismatch; execution authority is never changed in place.
func (s *Store) ClaimInteractionContinuation(ctx context.Context, tenantID, personID, sourceRunID, parentRunID string) (*Run, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("control store is unavailable")
	}
	tenantID = normalizeTenant(tenantID)
	personID = strings.TrimSpace(personID)
	sourceRunID = strings.TrimSpace(sourceRunID)
	parentRunID = strings.TrimSpace(parentRunID)
	if personID == "" || sourceRunID == "" || parentRunID == "" || sourceRunID == parentRunID {
		return nil, fmt.Errorf("person, interaction run, and distinct parent run are required")
	}
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = s.claimInteractionContinuationOnce(ctx, tenantID, personID, sourceRunID, parentRunID)
		if err == nil {
			return s.GetRun(ctx, tenantID, sourceRunID)
		}
		if !isSQLiteBusy(err) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(25*(attempt+1)) * time.Millisecond):
		}
	}
	return nil, err
}

func (s *Store) claimInteractionContinuationOnce(ctx context.Context, tenantID, personID, sourceRunID, parentRunID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	type runDomain struct {
		taskID, personID, workspaceID, rootsJSON, status, parentID string
	}
	load := func(runID string) (runDomain, error) {
		var domain runDomain
		err := tx.QueryRowContext(ctx, `SELECT task_id, person_id, COALESCE(workspace_id, ''),
			COALESCE(execution_roots_json, '[]'), status, COALESCE(parent_run_id, '')
			FROM task_runs WHERE tenant_id = ? AND id = ?`, tenantID, runID).
			Scan(&domain.taskID, &domain.personID, &domain.workspaceID, &domain.rootsJSON, &domain.status, &domain.parentID)
		return domain, err
	}
	source, err := load(sourceRunID)
	if err == sql.ErrNoRows {
		return fmt.Errorf("interaction run not found")
	}
	if err != nil {
		return err
	}
	parent, err := load(parentRunID)
	if err == sql.ErrNoRows {
		return fmt.Errorf("parent run not found")
	}
	if err != nil {
		return err
	}
	if source.personID != personID || parent.personID != personID {
		return fmt.Errorf("runs are unavailable for the current person")
	}
	if source.status != "running" || source.parentID != "" {
		return fmt.Errorf("interaction run is no longer eligible for a direct continuation claim")
	}
	var sourceRoots, parentRoots []executionenv.RootBinding
	if json.Unmarshal([]byte(source.rootsJSON), &sourceRoots) != nil || json.Unmarshal([]byte(parent.rootsJSON), &parentRoots) != nil ||
		source.workspaceID != parent.workspaceID || !reflect.DeepEqual(sourceRoots, parentRoots) {
		return ErrContinuationDomainMismatch
	}
	var checkpointCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM loop_checkpoints
		WHERE tenant_id = ? AND run_id = ? AND outcome <> 'complete_turn'`, tenantID, parentRunID).Scan(&checkpointCount); err != nil {
		return err
	}
	if checkpointCount > 0 {
		return ErrParentCheckpointRequired
	}
	var sourceRunCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_runs WHERE tenant_id = ? AND task_id = ?`, tenantID, source.taskID).Scan(&sourceRunCount); err != nil {
		return err
	}
	if sourceRunCount != 1 {
		return fmt.Errorf("direct continuation requires a single-run interaction task")
	}
	child := &Run{ID: sourceRunID, TenantID: tenantID, PersonID: personID, TaskID: parent.taskID, ParentRunID: parentRunID}
	if err := validateParentClaimTx(ctx, tx, child); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE task_runs SET task_id = ?, parent_run_id = ?
		WHERE tenant_id = ? AND person_id = ? AND id = ? AND status = 'running' AND parent_run_id = ''`,
		parent.taskID, parentRunID, tenantID, personID, sourceRunID)
	if err != nil {
		if strings.Contains(err.Error(), "idx_task_runs_parent_once") {
			return ErrParentRunClaimed
		}
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return fmt.Errorf("interaction run changed before continuation commit")
	}
	updates := []struct {
		query string
		args  []interface{}
	}{
		{`UPDATE task_events SET task_id = ? WHERE run_id = ?`, []interface{}{parent.taskID, sourceRunID}},
		{`UPDATE task_artifacts SET task_id = ? WHERE run_id = ?`, []interface{}{parent.taskID, sourceRunID}},
		{`UPDATE task_handoffs SET task_id = ? WHERE run_id = ?`, []interface{}{parent.taskID, sourceRunID}},
		{`UPDATE loop_checkpoints SET task_id = ? WHERE tenant_id = ? AND person_id = ? AND run_id = ?`, []interface{}{parent.taskID, tenantID, personID, sourceRunID}},
		{`UPDATE approval_requests SET task_id = ? WHERE tenant_id = ? AND person_id = ? AND run_id = ?`, []interface{}{parent.taskID, tenantID, personID, sourceRunID}},
		{`UPDATE clarify_requests SET task_id = ? WHERE tenant_id = ? AND person_id = ? AND run_id = ?`, []interface{}{parent.taskID, tenantID, personID, sourceRunID}},
		{`UPDATE task_blockers SET task_id = ? WHERE tenant_id = ? AND person_id = ? AND origin_run_id = ?`, []interface{}{parent.taskID, tenantID, personID, sourceRunID}},
		{`UPDATE steering_mailbox SET task_id = ? WHERE tenant_id = ? AND person_id = ? AND run_id = ?`, []interface{}{parent.taskID, tenantID, personID, sourceRunID}},
		{`UPDATE workflow_profiles SET task_id = ? WHERE tenant_id = ? AND person_id = ? AND run_id = ?`, []interface{}{parent.taskID, tenantID, personID, sourceRunID}},
	}
	for _, update := range updates {
		if _, err := tx.ExecContext(ctx, update.query, update.args...); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE run_work_units SET primary_task_id = ?,
		related_task_id = CASE WHEN related_task_id = ? THEN ? ELSE related_task_id END
		WHERE identity_tenant_id = ? AND run_id = ?`, parent.taskID, source.taskID, parent.taskID, tenantID, sourceRunID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE run_skill_activations SET primary_task_id = ?,
		related_task_id = CASE WHEN related_task_id = ? THEN ? ELSE related_task_id END
		WHERE identity_tenant_id = ? AND run_id = ?`, parent.taskID, source.taskID, parent.taskID, tenantID, sourceRunID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE channel_messages SET task_id = ?
		WHERE tenant_id = ? AND person_id = ? AND task_id = ?`, parent.taskID, tenantID, personID, source.taskID); err != nil {
		return err
	}
	if err := resolveOriginRunBlockersTx(ctx, tx, tenantID, parent.taskID, parentRunID, sourceRunID); err != nil {
		return err
	}
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET active_run_id = ?, status = 'running', archived_at = NULL,
		last_activity_at = ?, updated_at = ? WHERE tenant_id = ? AND person_id = ? AND id = ?`,
		sourceRunID, now, now, tenantID, personID, parent.taskID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE current_task SET task_id = ?, updated_at = ?
		WHERE tenant_id = ? AND person_id = ? AND task_id = ?`, parent.taskID, now, tenantID, personID, source.taskID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tasks WHERE tenant_id = ? AND person_id = ? AND id = ?
		AND NOT EXISTS (SELECT 1 FROM task_runs WHERE tenant_id = ? AND task_id = ?)
		AND NOT EXISTS (SELECT 1 FROM task_references WHERE tenant_id = ? AND task_id = ?)`,
		tenantID, personID, source.taskID, tenantID, source.taskID, tenantID, source.taskID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status = 'archived', active_run_id = NULL, archived_at = ?, updated_at = ?
		WHERE tenant_id = ? AND person_id = ? AND id = ? AND NOT EXISTS
		(SELECT 1 FROM task_runs WHERE tenant_id = ? AND task_id = ?)`,
		now, now, tenantID, personID, source.taskID, tenantID, source.taskID); err != nil {
		return err
	}
	return tx.Commit()
}

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
			JOIN task_runs r ON r.id = a.run_id AND r.task_id = a.task_id
			WHERE r.tenant_id = ? AND r.person_id = ? AND r.id = ?`},
		{"handoff", `SELECT COUNT(*) FROM task_handoffs h
			JOIN task_runs r ON r.id = h.run_id AND r.task_id = h.task_id
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
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_runs
		WHERE tenant_id = ? AND person_id = ? AND task_id = ?`, tenantID, personID, taskID).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("interaction projection requires a single-run task")
	}
	var exact int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_runs
		WHERE tenant_id = ? AND person_id = ? AND task_id = ? AND id = ?`, tenantID, personID, taskID, runID).Scan(&exact); err != nil {
		return err
	}
	if exact != 1 {
		return fmt.Errorf("interaction run does not belong to the task")
	}
	result, err := tx.ExecContext(ctx, `UPDATE tasks SET kind = 'interaction', visibility = 'hidden', updated_at = ?
		WHERE tenant_id = ? AND person_id = ? AND id = ?`, time.Now().Unix(), tenantID, personID, taskID)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return fmt.Errorf("interaction task not found")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM current_task WHERE tenant_id = ? AND person_id = ? AND task_id = ?`, tenantID, personID, taskID); err != nil {
		return err
	}
	return tx.Commit()
}
