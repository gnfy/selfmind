package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// resumableRunStatusSQL is the one SQL surface naming which run statuses a
// deliberate continuation may still own. The read-only parent-resolution
// queries and the child-creation claim validation must agree on it, or ingress
// could offer a parent the claim step then refuses.
const resumableRunStatusSQL = `('interrupted', 'waiting_user', 'verification_partial', 'blocked')`

// ErrParentRunClaimed reports a lost continuation race: another child claimed
// the parent between resolution and creation, or the parent left the
// resumable set. The caller must surface "already claimed" instead of forking
// a second continuation.
var ErrParentRunClaimed = errors.New("parent run is already claimed by another continuation")

// ErrParentRunNotResumable reports a parent that exists but cannot be
// continued (terminal or still running).
var ErrParentRunNotResumable = errors.New("parent run is not in a resumable state")

// validateParentClaimTx enforces the continuation invariants inside the
// child-creation transaction: the parent exists, agrees with the child on
// tenant/person/task, is resumable, and is unclaimed on BOTH edges (the
// forward parent_run_id edge and the legacy read-only resumed_by_run_id).
// The unique partial index idx_task_runs_parent_once remains the cross-process
// backstop for the race this check cannot see.
func validateParentClaimTx(ctx context.Context, tx *sql.Tx, child *Run) error {
	var taskID, personID, status, legacyClaim string
	err := tx.QueryRowContext(ctx,
		`SELECT thread_id, person_id, status, COALESCE(resumed_by_run_id, '')
		 FROM runs WHERE tenant_id = ? AND id = ?`,
		child.TenantID, child.ParentRunID).
		Scan(&taskID, &personID, &status, &legacyClaim)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("parent run %s not found", child.ParentRunID)
	}
	if err != nil {
		return err
	}
	if taskID != child.TaskID || personID != child.PersonID {
		return fmt.Errorf("parent run %s belongs to a different task or person", child.ParentRunID)
	}
	switch status {
	case "interrupted", "waiting_user", "verification_partial", "blocked":
	case "waiting_external":
		// A Run parked on a daemon watcher becomes claimable only once every
		// watcher it registered has concluded: the watcher finalization is then
		// the continuation that records the verdict as this Run's exact child.
		// While a watcher is still live nothing may steal the Run from it.
		var live int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM external_watches
			 WHERE tenant_id = ? AND run_id = ? AND status IN ('pending', 'running')`,
			child.TenantID, child.ParentRunID).Scan(&live); err != nil {
			return err
		}
		if live > 0 {
			return ErrParentRunNotResumable
		}
	default:
		return ErrParentRunNotResumable
	}
	if legacyClaim != "" {
		return ErrParentRunClaimed
	}
	var claimed int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runs WHERE tenant_id = ? AND parent_run_id = ?`,
		child.TenantID, child.ParentRunID).Scan(&claimed); err != nil {
		return err
	}
	if claimed > 0 {
		return ErrParentRunClaimed
	}
	return nil
}

// ListUnresolvedRuns returns a task's resumable runs that no continuation has
// claimed yet, newest first. It is the read-only parent-resolution input used
// BEFORE a run is created: exactly one row means an exact parent; more means
// ambiguity that must stay visible instead of being guessed away.
func (s *Store) ListUnresolvedRuns(ctx context.Context, tenantID, personID, taskID string, limit int) ([]Run, error) {
	if strings.TrimSpace(personID) == "" || strings.TrimSpace(taskID) == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 20 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, thread_id, tenant_id, person_id, COALESCE(workspace_id, ''),
		        COALESCE(execution_roots_json, '[]'), channel, COALESCE(input_summary, ''),
		        COALESCE(work_key, ''), COALESCE(parent_run_id, ''), status, started_at, finished_at
		 FROM runs
			 WHERE tenant_id = ? AND person_id = ? AND thread_id = ?
		   AND status IN `+resumableRunStatusSQL+`
		   AND COALESCE(resumed_by_run_id, '') = ''
		   AND NOT EXISTS (
		       SELECT 1 FROM runs child
		        WHERE child.tenant_id = runs.tenant_id
		          AND child.parent_run_id = runs.id)
		 ORDER BY started_at DESC, id DESC LIMIT ?`,
		normalizeTenant(tenantID), personID, taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var started int64
		var finished sql.NullInt64
		var rootsJSON string
		if err := rows.Scan(&r.ID, &r.TaskID, &r.TenantID, &r.PersonID, &r.WorkspaceID,
			&rootsJSON, &r.Channel, &r.InputSummary, &r.WorkKey, &r.ParentRunID, &r.Status, &started, &finished); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(rootsJSON), &r.ExecutionRoots)
		r.StartedAt = time.Unix(started, 0)
		if finished.Valid {
			at := time.Unix(finished.Int64, 0)
			r.FinishedAt = &at
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListUnresolvedRunsForPerson returns the person's unclaimed resumable runs
// across ALL visible tasks, newest first, optionally narrowed to one channel.
// It backs the §5.3 continuation ladder: a deliberate cue continues the unique
// same-channel candidate, then the unique person-global candidate; several
// stay visible as candidates and none falls through to a fresh root task.
// Hidden/archived labels are excluded — implicit continuation must not
// resurrect deliberately shelved work.
func (s *Store) ListUnresolvedRunsForPerson(ctx context.Context, tenantID, personID, channel string, limit int) ([]Run, error) {
	return s.listUnresolvedRunsForPerson(ctx, tenantID, personID, channel, limit, false)
}

// ListExplicitlyResumableRunsForPerson is the lookup surface for a user who
// supplied a Run reference. Unlike the implicit continuation ladder it keeps
// dismissed and archived history addressable: presentation choices may quiet
// work, but they never destroy an explicit continuation control.
func (s *Store) ListExplicitlyResumableRunsForPerson(ctx context.Context, tenantID, personID string, limit int) ([]Run, error) {
	return s.listUnresolvedRunsForPerson(ctx, tenantID, personID, "", limit, true)
}

func (s *Store) listUnresolvedRunsForPerson(ctx context.Context, tenantID, personID, channel string, limit int, explicit bool) ([]Run, error) {
	if strings.TrimSpace(personID) == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 10
	}
	query := `SELECT r.id, r.thread_id, r.tenant_id, r.person_id, COALESCE(r.workspace_id, ''),
		        COALESCE(r.execution_roots_json, '[]'), r.channel, COALESCE(r.input_summary, ''),
		        COALESCE(r.work_key, ''), COALESCE(r.parent_run_id, ''), r.status, r.started_at, r.finished_at
		 FROM runs r
		 JOIN threads t ON t.tenant_id = r.tenant_id AND t.id = r.thread_id
		 WHERE r.tenant_id = ? AND r.person_id = ?
		   AND r.status IN ` + resumableRunStatusSQL + `
		   AND COALESCE(r.resumed_by_run_id, '') = ''
		   AND NOT EXISTS (
		       SELECT 1 FROM runs child
		        WHERE child.tenant_id = r.tenant_id
		          AND child.parent_run_id = r.id)`
	args := []any{normalizeTenant(tenantID), strings.TrimSpace(personID)}
	if !explicit {
		query += ` AND COALESCE(r.attention_dismissed_at, 0) = 0
		   AND COALESCE(t.visibility, 'visible') NOT IN ('hidden', 'archived')`
	}
	if strings.TrimSpace(channel) != "" {
		query += ` AND r.channel = ?`
		args = append(args, strings.TrimSpace(channel))
	}
	query += ` ORDER BY r.started_at DESC, r.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var started int64
		var finished sql.NullInt64
		var rootsJSON string
		if err := rows.Scan(&r.ID, &r.TaskID, &r.TenantID, &r.PersonID, &r.WorkspaceID,
			&rootsJSON, &r.Channel, &r.InputSummary, &r.WorkKey, &r.ParentRunID, &r.Status, &started, &finished); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(rootsJSON), &r.ExecutionRoots)
		r.StartedAt = time.Unix(started, 0)
		if finished.Valid {
			at := time.Unix(finished.Int64, 0)
			r.FinishedAt = &at
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunHandoff returns the handoff recorded when one run finalized, or nil when
// that run never reached finalization. The formal run key is the v7 column
// (backfilled from the deterministic "handoff_run_<run_id>" primary key the
// production finalizer always used).
func (s *Store) RunHandoff(ctx context.Context, tenantID, personID, runID string) (*Handoff, error) {
	if strings.TrimSpace(personID) == "" || strings.TrimSpace(runID) == "" {
		return nil, nil
	}
	var h Handoff
	var doneJSON, nextJSON, filesJSON, risksJSON string
	var created int64
	err := s.db.QueryRowContext(ctx,
		`SELECT h.id, h.thread_id, COALESCE(h.run_id, ''), h.summary, COALESCE(h.done_items_json, '[]'), COALESCE(h.next_steps_json, '[]'),
		        COALESCE(h.changed_files_json, '[]'), COALESCE(h.test_status, ''), COALESCE(h.risks_json, '[]'), h.created_at
		 FROM task_handoffs h JOIN runs r ON r.id = h.run_id AND r.thread_id = h.thread_id
		 WHERE r.tenant_id = ? AND r.person_id = ? AND r.id = ? ORDER BY h.created_at DESC LIMIT 1`,
		normalizeTenant(tenantID), personID, runID).
		Scan(&h.ID, &h.TaskID, &h.RunID, &h.Summary, &doneJSON, &nextJSON, &filesJSON, &h.TestStatus, &risksJSON, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(doneJSON), &h.DoneItems)
	_ = json.Unmarshal([]byte(nextJSON), &h.NextSteps)
	_ = json.Unmarshal([]byte(filesJSON), &h.ChangedFiles)
	_ = json.Unmarshal([]byte(risksJSON), &h.Risks)
	h.CreatedAt = time.Unix(created, 0)
	return &h, nil
}

// ListRunEvents mirrors ListTaskEvents scoped to exactly one run, so a
// continuation's context can carry the parent run's own events instead of the
// whole task history.
func (s *Store) ListRunEvents(ctx context.Context, tenantID, personID, taskID, runID string, limit int) ([]Event, error) {
	if strings.TrimSpace(personID) == "" || strings.TrimSpace(taskID) == "" || strings.TrimSpace(runID) == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT e.cursor, e.id, e.thread_id, COALESCE(e.run_id, ''), e.type, e.visibility, COALESCE(e.channel, ''),
		        COALESCE(e.payload_json, '{}'), e.created_at
		 FROM task_events e JOIN runs r ON r.id = e.run_id AND r.thread_id = e.thread_id
		 WHERE r.tenant_id = ? AND r.person_id = ? AND e.thread_id = ? AND e.run_id = ?
		 ORDER BY e.cursor DESC LIMIT ?`,
		normalizeTenant(tenantID), personID, taskID, runID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var payload string
		var created int64
		if err := rows.Scan(&e.Cursor, &e.ID, &e.TaskID, &e.RunID, &e.Type, &e.Visibility, &e.Channel, &payload, &created); err != nil {
			return nil, err
		}
		e.Payload = json.RawMessage(payload)
		e.CreatedAt = time.Unix(created, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListRunArtifacts mirrors ListTaskArtifacts scoped to exactly one run.
func (s *Store) ListRunArtifacts(ctx context.Context, tenantID, personID, taskID, runID string, limit int) ([]Artifact, error) {
	if strings.TrimSpace(personID) == "" || strings.TrimSpace(taskID) == "" || strings.TrimSpace(runID) == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT a.id, a.thread_id, COALESCE(a.run_id, ''), a.kind, COALESCE(a.name, ''),
		        a.uri, COALESCE(a.mime_type, ''), COALESCE(a.metadata_json, '{}'), a.created_at
		 FROM task_artifacts a JOIN runs r ON r.id = a.run_id AND r.thread_id = a.thread_id
		 WHERE r.tenant_id = ? AND r.person_id = ? AND a.thread_id = ? AND a.run_id = ?
		 ORDER BY a.created_at DESC, a.id DESC LIMIT ?`,
		normalizeTenant(tenantID), personID, taskID, runID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Artifact
	for rows.Next() {
		var item Artifact
		var metadata string
		var created int64
		if err := rows.Scan(&item.ID, &item.TaskID, &item.RunID, &item.Kind, &item.Name,
			&item.URI, &item.MimeType, &metadata, &created); err != nil {
			return nil, err
		}
		item.Metadata = json.RawMessage(metadata)
		item.CreatedAt = time.Unix(created, 0)
		out = append(out, item)
	}
	return out, rows.Err()
}
