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

// ErrResumeTargetClaimed reports a lost continuation race: another child claimed
// the parent between resolution and creation, or the parent left the
// resumable set. The caller must surface "already claimed" instead of forking
// a second continuation.
var ErrResumeTargetClaimed = errors.New("parent run is already claimed by another continuation")

// ErrResumeTargetNotResumable reports a parent that exists but cannot be
// continued (terminal or still running).
var ErrResumeTargetNotResumable = errors.New("parent run is not in a resumable state")

// validateResumeClaimTx enforces the continuation invariants inside the
// child-creation transaction: the parent exists, agrees with the child on
// tenant/person/task, is resumable, and is unclaimed on BOTH edges (the
// forward resumes_run_id edge and the legacy read-only resumed_by_run_id).
// The unique partial index idx_task_runs_parent_once remains the cross-process
// backstop for the race this check cannot see.
func validateResumeClaimTx(ctx context.Context, tx *sql.Tx, child *Run) error {
	var taskID, personID, status, legacyClaim string
	err := tx.QueryRowContext(ctx,
		`SELECT thread_id, person_id, status, COALESCE(resumed_by_run_id, '')
		 FROM runs WHERE tenant_id = ? AND id = ?`,
		child.TenantID, child.ResumesRunID).
		Scan(&taskID, &personID, &status, &legacyClaim)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("parent run %s not found", child.ResumesRunID)
	}
	if err != nil {
		return err
	}
	if taskID != child.TaskID || personID != child.PersonID {
		return fmt.Errorf("parent run %s belongs to a different task or person", child.ResumesRunID)
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
			child.TenantID, child.ResumesRunID).Scan(&live); err != nil {
			return err
		}
		if live > 0 {
			return ErrResumeTargetNotResumable
		}
	default:
		return ErrResumeTargetNotResumable
	}
	if legacyClaim != "" {
		return ErrResumeTargetClaimed
	}
	var claimed int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runs WHERE tenant_id = ? AND resumes_run_id = ?`,
		child.TenantID, child.ResumesRunID).Scan(&claimed); err != nil {
		return err
	}
	if claimed > 0 {
		return ErrResumeTargetClaimed
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
		        COALESCE(work_key, ''), COALESCE(resumes_run_id, ''), status, started_at, finished_at
		 FROM runs
			 WHERE tenant_id = ? AND person_id = ? AND thread_id = ?
		   AND status IN `+resumableRunStatusSQL+`
		   AND COALESCE(resumed_by_run_id, '') = ''
		   AND NOT EXISTS (
		       SELECT 1 FROM runs child
		        WHERE child.tenant_id = runs.tenant_id
		          AND child.resumes_run_id = runs.id)
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
			&rootsJSON, &r.Channel, &r.InputSummary, &r.WorkKey, &r.ResumesRunID, &r.Status, &started, &finished); err != nil {
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
		        COALESCE(r.work_key, ''), COALESCE(r.resumes_run_id, ''), r.status, r.started_at, r.finished_at
		 FROM runs r
		 WHERE r.tenant_id = ? AND r.person_id = ?
		   AND r.status IN ` + resumableRunStatusSQL + `
		   AND COALESCE(r.resumed_by_run_id, '') = ''
		   AND NOT EXISTS (
		       SELECT 1 FROM runs child
		        WHERE child.tenant_id = r.tenant_id
		          AND child.resumes_run_id = r.id)`
	args := []any{normalizeTenant(tenantID), strings.TrimSpace(personID)}
	if !explicit {
		query += ` AND COALESCE(r.attention_dismissed_at, 0) = 0`
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
			&rootsJSON, &r.Channel, &r.InputSummary, &r.WorkKey, &r.ResumesRunID, &r.Status, &started, &finished); err != nil {
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

// resumeChainMaxHops bounds an upward walk of the resume edge. A chain is a
// handful of hops in practice (measured: about 5% of runs carry an edge at
// all), and the unique index forbids a fork, so the bound exists only to keep a
// corrupted cycle from spinning.
const resumeChainMaxHops = 16

// ResumeChainRoot returns the oldest run that runID transitively resumes, or
// runID itself when it resumes nothing.
//
// This is the read-time replacement for what Task used to provide: "which runs
// are one line of work". Task answered it with a judgment applied when the work
// began, before anyone knew — and that judgment demonstrably mis-grouped
// unrelated runs. The resume edge answers it with a fact recorded when a person
// or the daemon actually continued something, so the grouping cannot be wrong;
// it can only be absent, which is the honest answer for a run that stands alone.
func (s *Store) ResumeChainRoot(ctx context.Context, tenantID, runID string) (string, error) {
	tenantID = normalizeTenant(tenantID)
	current := strings.TrimSpace(runID)
	if current == "" {
		return "", nil
	}
	seen := map[string]struct{}{current: {}}
	for hop := 0; hop < resumeChainMaxHops; hop++ {
		var next string
		err := s.db.QueryRowContext(ctx,
			`SELECT COALESCE(resumes_run_id, '') FROM runs WHERE tenant_id = ? AND id = ?`,
			tenantID, current).Scan(&next)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return current, nil
			}
			return "", err
		}
		next = strings.TrimSpace(next)
		if next == "" {
			return current, nil
		}
		if _, cycle := seen[next]; cycle {
			return current, nil
		}
		seen[next] = struct{}{}
		current = next
	}
	return current, nil
}

// ResumeChainRunIDs returns runID plus every run it transitively resumes,
// oldest last. Callers use it to treat one line of continued work as a unit
// without inventing a container for it.
func (s *Store) ResumeChainRunIDs(ctx context.Context, tenantID, runID string) ([]string, error) {
	tenantID = normalizeTenant(tenantID)
	current := strings.TrimSpace(runID)
	if current == "" {
		return nil, nil
	}
	chain := []string{current}
	seen := map[string]struct{}{current: {}}
	for hop := 0; hop < resumeChainMaxHops; hop++ {
		var next string
		err := s.db.QueryRowContext(ctx,
			`SELECT COALESCE(resumes_run_id, '') FROM runs WHERE tenant_id = ? AND id = ?`,
			tenantID, current).Scan(&next)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return chain, nil
			}
			return nil, err
		}
		next = strings.TrimSpace(next)
		if next == "" {
			return chain, nil
		}
		if _, cycle := seen[next]; cycle {
			return chain, nil
		}
		seen[next] = struct{}{}
		chain = append(chain, next)
		current = next
	}
	return chain, nil
}

// ResumeLineRunIDs returns every Run in one line of work: the run given, every
// run it transitively resumes, and every run that transitively resumes it.
//
// This is the replacement for "the runs of this Task". Task answered that
// question with a judgment made when the work began — before anyone knew what
// the work was — and that judgment demonstrably swept unrelated runs together.
// The resume edge answers it with a fact recorded only when something was
// actually continued, so a line can be incomplete but never wrong. The unique
// index forbids a fork, so a line is a simple chain.
//
// The result is ordered oldest first and always contains runID itself, so a
// caller can use it as an IN-list without a special case for standalone work.
func (s *Store) ResumeLineRunIDs(ctx context.Context, tenantID, runID string) ([]string, error) {
	tenantID = normalizeTenant(tenantID)
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return nil, nil
	}
	upward, err := s.ResumeChainRunIDs(ctx, tenantID, runID)
	if err != nil {
		return nil, err
	}
	line := make([]string, 0, len(upward)+2)
	for i := len(upward) - 1; i >= 0; i-- {
		line = append(line, upward[i])
	}
	seen := make(map[string]struct{}, len(line))
	for _, id := range line {
		seen[id] = struct{}{}
	}
	current := runID
	for hop := 0; hop < resumeChainMaxHops; hop++ {
		var next string
		err := s.db.QueryRowContext(ctx,
			`SELECT id FROM runs WHERE tenant_id = ? AND resumes_run_id = ?`,
			tenantID, current).Scan(&next)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			return nil, err
		}
		next = strings.TrimSpace(next)
		if next == "" {
			break
		}
		if _, cycle := seen[next]; cycle {
			break
		}
		seen[next] = struct{}{}
		line = append(line, next)
		current = next
	}
	return line, nil
}

// resumeLinePlaceholders renders an IN-list for a work line.
func resumeLinePlaceholders(ids []string) (string, []any) {
	if len(ids) == 0 {
		return "('')", nil
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	return "(" + placeholders(len(ids)) + ")", args
}
