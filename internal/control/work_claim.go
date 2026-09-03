package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"selfmind/internal/executionenv"
)

// ErrContinuationDomainMismatch reports that the interaction Run and the
// selected parent Run do not share one frozen execution domain (workspace and
// execution roots). Execution authority never changes in place, so the caller
// must continue through a transfer child instead.
var ErrContinuationDomainMismatch = errors.New("interaction and parent runs are in different execution domains")

// ErrParentCheckpointRequired reports that the parent Run stopped inside an
// unfinished loop checkpoint. Only a fresh child kernel can restore that
// checkpoint, so the continuation must be a transfer child.
var ErrParentCheckpointRequired = errors.New("parent run requires checkpoint restoration in a fresh run")

// ClaimInteractionContinuation turns a still-running, effect-free interaction
// Run into the direct child of one exact historical Run when both already share
// the same frozen execution domain. It runs at work_select time so the same
// Main turn continues the selected work: parent ownership, run/thread
// re-pointing, dependent rows, blocker settlement, and placeholder cleanup
// commit together. A domain or checkpoint mismatch is reported with a typed
// error and the caller creates a transfer child instead.
func (s *Store) ClaimInteractionContinuation(ctx context.Context, tenantID, personID, sourceRunID, parentRunID string) (*Run, error) {
	return s.continuationClaimWithRetry(ctx, tenantID, personID, sourceRunID, parentRunID, false)
}

// RetargetInteractionContinuation moves a running interaction that already
// claimed one parent in this turn onto a different same-domain parent. It is
// the pre-effect correction path; the caller has verified the run produced no
// material effect. The previous parent becomes unclaimed again because the
// edge lives on the child, and the run's dependent rows follow it.
func (s *Store) RetargetInteractionContinuation(ctx context.Context, tenantID, personID, runID, newParentRunID string) (*Run, error) {
	return s.continuationClaimWithRetry(ctx, tenantID, personID, runID, newParentRunID, true)
}

func (s *Store) continuationClaimWithRetry(ctx context.Context, tenantID, personID, sourceRunID, parentRunID string, retarget bool) (*Run, error) {
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
		err = s.claimInteractionContinuationOnce(ctx, tenantID, personID, sourceRunID, parentRunID, retarget)
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

type continuationRunDomain struct {
	threadID, personID, workspaceID, rootsJSON, status, parentID string
}

func loadContinuationRunDomainTx(ctx context.Context, tx *sql.Tx, tenantID, runID string) (continuationRunDomain, error) {
	var domain continuationRunDomain
	err := tx.QueryRowContext(ctx, `SELECT thread_id, person_id, COALESCE(workspace_id, ''),
		COALESCE(execution_roots_json, '[]'), status, COALESCE(parent_run_id, '')
		FROM runs WHERE tenant_id = ? AND id = ?`, tenantID, runID).
		Scan(&domain.threadID, &domain.personID, &domain.workspaceID, &domain.rootsJSON, &domain.status, &domain.parentID)
	return domain, err
}

func (s *Store) claimInteractionContinuationOnce(ctx context.Context, tenantID, personID, sourceRunID, parentRunID string, retarget bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	source, err := loadContinuationRunDomainTx(ctx, tx, tenantID, sourceRunID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("interaction run not found")
	}
	if err != nil {
		return err
	}
	parent, err := loadContinuationRunDomainTx(ctx, tx, tenantID, parentRunID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("parent run not found")
	}
	if err != nil {
		return err
	}
	if source.personID != personID || parent.personID != personID {
		return fmt.Errorf("runs are unavailable for the current person")
	}
	if source.status != "running" {
		return fmt.Errorf("interaction run is no longer eligible for a direct continuation claim")
	}
	if retarget {
		if source.parentID == "" || source.parentID == parentRunID {
			return fmt.Errorf("interaction run has no different parent to correct")
		}
	} else if source.parentID != "" {
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
	if !retarget {
		var sourceRunCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE tenant_id = ? AND thread_id = ?`, tenantID, source.threadID).Scan(&sourceRunCount); err != nil {
			return err
		}
		if sourceRunCount != 1 {
			return fmt.Errorf("direct continuation requires a single-run interaction thread")
		}
	}
	child := &Run{ID: sourceRunID, TenantID: tenantID, PersonID: personID, TaskID: parent.threadID, ParentRunID: parentRunID}
	if err := validateParentClaimTx(ctx, tx, child); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE runs SET thread_id = ?, parent_run_id = ?
		WHERE tenant_id = ? AND person_id = ? AND id = ? AND status = 'running' AND COALESCE(parent_run_id, '') = ?`,
		parent.threadID, parentRunID, tenantID, personID, sourceRunID, source.parentID)
	if err != nil {
		if strings.Contains(err.Error(), "parent_once") {
			return ErrParentRunClaimed
		}
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return fmt.Errorf("interaction run changed before continuation commit")
	}
	if err := moveRunDependentsTx(ctx, tx, tenantID, personID, sourceRunID, source.threadID, parent.threadID); err != nil {
		return err
	}
	if err := resolveOriginRunBlockersTx(ctx, tx, tenantID, parent.threadID, parentRunID, sourceRunID); err != nil {
		return err
	}
	now := time.Now().Unix()
	// The person deliberately continued this work: the parent thread is
	// listed work again even if it had been archived, mirroring explicit
	// /resume. Presentation only; execution authority already matched.
	if _, err := tx.ExecContext(ctx, `UPDATE threads SET kind = 'work', visibility = 'listed',
		last_activity_at = ?, updated_at = ? WHERE tenant_id = ? AND person_id = ? AND id = ?`,
		now, now, tenantID, personID, parent.threadID); err != nil {
		return err
	}
	if retarget {
		if _, err := tx.ExecContext(ctx, `UPDATE threads SET updated_at = ? WHERE tenant_id = ? AND person_id = ? AND id = ?`,
			now, tenantID, personID, source.threadID); err != nil {
			return err
		}
		return tx.Commit()
	}
	// The interaction placeholder is empty now. Remove it unless a governed
	// reference still points at it; then keep it as unlisted history.
	if _, err := tx.ExecContext(ctx, `DELETE FROM threads WHERE tenant_id = ? AND person_id = ? AND id = ?
		AND NOT EXISTS (SELECT 1 FROM runs WHERE tenant_id = ? AND thread_id = ?)
		AND NOT EXISTS (SELECT 1 FROM task_references WHERE tenant_id = ? AND thread_id = ?)`,
		tenantID, personID, source.threadID, tenantID, source.threadID, tenantID, source.threadID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE threads SET visibility = 'unlisted', updated_at = ?
		WHERE tenant_id = ? AND person_id = ? AND id = ? AND NOT EXISTS
		(SELECT 1 FROM runs WHERE tenant_id = ? AND thread_id = ?)`,
		now, tenantID, personID, source.threadID, tenantID, source.threadID); err != nil {
		return err
	}
	return tx.Commit()
}

// moveRunDependentsTx re-points every row that belongs to one run from its old
// thread to the thread it now continues. Rows keyed only by thread (channel
// messages of the interaction placeholder) follow on the first claim, when the
// old thread held nothing but this run.
func moveRunDependentsTx(ctx context.Context, tx *sql.Tx, tenantID, personID, runID, oldThreadID, newThreadID string) error {
	updates := []struct {
		query string
		args  []interface{}
	}{
		{`UPDATE task_events SET thread_id = ? WHERE run_id = ?`, []interface{}{newThreadID, runID}},
		{`UPDATE task_artifacts SET thread_id = ? WHERE run_id = ?`, []interface{}{newThreadID, runID}},
		{`UPDATE task_handoffs SET thread_id = ? WHERE run_id = ?`, []interface{}{newThreadID, runID}},
		{`UPDATE loop_checkpoints SET thread_id = ? WHERE tenant_id = ? AND person_id = ? AND run_id = ?`, []interface{}{newThreadID, tenantID, personID, runID}},
		{`UPDATE approval_requests SET thread_id = ? WHERE tenant_id = ? AND person_id = ? AND run_id = ?`, []interface{}{newThreadID, tenantID, personID, runID}},
		{`UPDATE clarify_requests SET thread_id = ? WHERE tenant_id = ? AND person_id = ? AND run_id = ?`, []interface{}{newThreadID, tenantID, personID, runID}},
		{`UPDATE task_blockers SET thread_id = ? WHERE tenant_id = ? AND person_id = ? AND origin_run_id = ?`, []interface{}{newThreadID, tenantID, personID, runID}},
		{`UPDATE steering_mailbox SET thread_id = ? WHERE tenant_id = ? AND person_id = ? AND run_id = ?`, []interface{}{newThreadID, tenantID, personID, runID}},
		{`UPDATE workflow_profiles SET thread_id = ? WHERE tenant_id = ? AND person_id = ? AND run_id = ?`, []interface{}{newThreadID, tenantID, personID, runID}},
		{`UPDATE channel_messages SET thread_id = ? WHERE tenant_id = ? AND person_id = ? AND thread_id = ?
			AND NOT EXISTS (SELECT 1 FROM runs WHERE tenant_id = ? AND thread_id = ? AND id <> ?)`,
			[]interface{}{newThreadID, tenantID, personID, oldThreadID, tenantID, oldThreadID, runID}},
	}
	for _, update := range updates {
		if _, err := tx.ExecContext(ctx, update.query, update.args...); err != nil {
			return err
		}
	}
	for _, table := range []string{"run_work_units", "run_skill_activations"} {
		if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET primary_task_id = ?,
			related_task_id = CASE WHEN related_task_id = ? THEN ? ELSE related_task_id END
			WHERE identity_tenant_id = ? AND run_id = ?`, newThreadID, oldThreadID, newThreadID, tenantID, runID); err != nil {
			return err
		}
	}
	return nil
}

// threadIDForRun returns the thread a run currently belongs to, or "" when the
// run is unknown. Control objects created for a run derive their thread from
// it: a tool scope frozen at run start still names the interaction placeholder
// after a same-domain direct claim moved the run onto the continued thread, and
// an approval, clarification, or watcher filed under that stale id would never
// join the run in Attention.
func (s *Store) threadIDForRun(ctx context.Context, tenantID, runID string) string {
	if s == nil || s.db == nil || strings.TrimSpace(runID) == "" {
		return ""
	}
	var threadID string
	if err := s.db.QueryRowContext(ctx, `SELECT thread_id FROM runs WHERE tenant_id = ? AND id = ?`,
		normalizeTenant(tenantID), strings.TrimSpace(runID)).Scan(&threadID); err != nil {
		return ""
	}
	return strings.TrimSpace(threadID)
}
