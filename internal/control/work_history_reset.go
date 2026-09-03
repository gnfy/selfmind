package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var ErrLiveWorkPreventsReset = errors.New("live work prevents work-history reset")

// WorkHistoryResetPreview is deliberately aggregate-only: the maintenance
// command must show the blast radius without printing user content.
type WorkHistoryResetPreview struct {
	Threads       int
	Runs          int
	QueueRows     int
	LiveRuns      int
	LiveWatchers  int
	StartedQueues int
}

func (p WorkHistoryResetPreview) HasLiveWork() bool {
	return p.LiveRuns+p.LiveWatchers+p.StartedQueues > 0
}

// PreviewWorkHistoryReset reports the current tenant-scoped reset surface.
func (s *Store) PreviewWorkHistoryReset(ctx context.Context, tenantID string) (WorkHistoryResetPreview, error) {
	return previewWorkHistoryReset(ctx, s.db, normalizeTenant(tenantID))
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func previewWorkHistoryReset(ctx context.Context, q rowQuerier, tenantID string) (WorkHistoryResetPreview, error) {
	var out WorkHistoryResetPreview
	queries := []struct {
		dst   *int
		query string
		args  []any
	}{
		{&out.Threads, `SELECT COUNT(*) FROM threads WHERE tenant_id = ?`, []any{tenantID}},
		{&out.Runs, `SELECT COUNT(*) FROM runs WHERE tenant_id = ?`, []any{tenantID}},
		{&out.QueueRows, `SELECT COUNT(*) FROM task_queue WHERE tenant_id = ?`, []any{tenantID}},
		{&out.LiveRuns, `SELECT COUNT(*) FROM runs WHERE tenant_id = ? AND status = 'running'`, []any{tenantID}},
		{&out.LiveWatchers, `SELECT COUNT(*) FROM external_watches WHERE tenant_id = ? AND status IN ('pending', 'running')`, []any{tenantID}},
		{&out.StartedQueues, `SELECT COUNT(*) FROM task_queue WHERE tenant_id = ? AND status = 'started'`, []any{tenantID}},
	}
	for _, item := range queries {
		if err := q.QueryRowContext(ctx, item.query, item.args...).Scan(item.dst); err != nil {
			return WorkHistoryResetPreview{}, err
		}
	}
	return out, nil
}

// BackupWorkHistorySnapshot creates a verified SQLite snapshot immediately
// before destructive maintenance. It is separate from migration backups so
// retention of schema-upgrade recovery points cannot prune a user reset point.
func (s *Store) BackupWorkHistorySnapshot(ctx context.Context, dataDir string) (string, error) {
	if s == nil || s.db == nil {
		return "", fmt.Errorf("control store is unavailable")
	}
	dataDir = strings.TrimSpace(dataDir)
	if dataDir == "" {
		return "", fmt.Errorf("data dir is required")
	}
	backupDir := filepath.Join(dataDir, "backups")
	if err := os.MkdirAll(backupDir, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(backupDir, "control-before-work-history-reset-"+
		time.Now().UTC().Format("20060102T150405.000000000Z")+".db")
	quoted := strings.ReplaceAll(path, "'", "''")
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO '"+quoted+"'"); err != nil {
		return "", err
	}
	if err := os.Chmod(path, 0600); err != nil {
		return "", err
	}
	backup, err := sql.Open("sqlite", path)
	if err != nil {
		return "", err
	}
	defer backup.Close()
	if err := quickCheckDB(ctx, backup); err != nil {
		return "", fmt.Errorf("verify work-history backup: %w", err)
	}
	return path, nil
}

// ResetWorkHistory removes the current tenant's Thread/Run history and every
// dependent control record, including in-flight Skill learning evidence
// (workflow observations and profiles, candidate refs, attributions, run skill
// activations, and task skill bindings). Identity, accounts, workspaces,
// settings, grants, provider health, memory databases, and published Skill
// packages are untouched; package-side rows that cite removed evidence keep
// their frozen evidence and lose only the dangling references.
// The caller must create a backup first; this method enforces the live-work
// fence again inside the write transaction.
func (s *Store) ResetWorkHistory(ctx context.Context, tenantID string) (WorkHistoryResetPreview, error) {
	tenantID = normalizeTenant(tenantID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkHistoryResetPreview{}, err
	}
	defer tx.Rollback()

	// Acquire SQLite's writer reservation before the live-work check so another
	// process cannot start a Run between validation and deletion.
	if _, err := tx.ExecContext(ctx, `UPDATE threads SET updated_at = updated_at WHERE tenant_id = ? AND 0`, tenantID); err != nil {
		return WorkHistoryResetPreview{}, err
	}
	preview, err := previewWorkHistoryReset(ctx, tx, tenantID)
	if err != nil {
		return WorkHistoryResetPreview{}, err
	}
	if preview.HasLiveWork() {
		return preview, fmt.Errorf("%w: running runs=%d, live watchers=%d, started queue rows=%d",
			ErrLiveWorkPreventsReset, preview.LiveRuns, preview.LiveWatchers, preview.StartedQueues)
	}

	for _, statement := range []string{
		`DROP TABLE IF EXISTS temp.reset_thread_ids`,
		`DROP TABLE IF EXISTS temp.reset_run_ids`,
		`DROP TABLE IF EXISTS temp.reset_reference_ids`,
		`DROP TABLE IF EXISTS temp.reset_observation_ids`,
		`CREATE TEMP TABLE reset_thread_ids (id TEXT PRIMARY KEY)`,
		`INSERT INTO reset_thread_ids SELECT id FROM threads WHERE tenant_id = ?`,
		`CREATE TEMP TABLE reset_run_ids (id TEXT PRIMARY KEY)`,
		`INSERT INTO reset_run_ids SELECT id FROM runs WHERE tenant_id = ?`,
		`CREATE TEMP TABLE reset_reference_ids (id TEXT PRIMARY KEY)`,
		`INSERT INTO reset_reference_ids SELECT id FROM task_references WHERE tenant_id = ?`,
		`CREATE TEMP TABLE reset_observation_ids (id TEXT PRIMARY KEY)`,
		`INSERT INTO reset_observation_ids SELECT id FROM workflow_observations WHERE identity_tenant_id = ?`,
		`DELETE FROM task_reference_evidence WHERE reference_id IN (SELECT id FROM reset_reference_ids) OR run_id IN (SELECT id FROM reset_run_ids)`,
		`DELETE FROM task_resolution_events WHERE tenant_id = ?`,
		`DELETE FROM pending_turn_choices WHERE tenant_id = ?`,
		`DELETE FROM turn_resolution_events WHERE tenant_id = ?`,
		`DELETE FROM run_plan_steps WHERE tenant_id = ?`,
		`DELETE FROM run_plan_versions WHERE tenant_id = ?`,
		`DELETE FROM run_delivery_overrides WHERE tenant_id = ?`,
		`DELETE FROM execution_leases WHERE tenant_id = ? AND run_id IN (SELECT id FROM reset_run_ids)`,
		`DELETE FROM task_events WHERE thread_id IN (SELECT id FROM reset_thread_ids)`,
		`DELETE FROM channel_messages WHERE tenant_id = ? AND thread_id IN (SELECT id FROM reset_thread_ids)`,
		`DELETE FROM task_handoffs WHERE thread_id IN (SELECT id FROM reset_thread_ids)`,
		`DELETE FROM task_artifacts WHERE thread_id IN (SELECT id FROM reset_thread_ids)`,
		`DELETE FROM task_blockers WHERE tenant_id = ? AND thread_id IN (SELECT id FROM reset_thread_ids)`,
		`DELETE FROM approval_requests WHERE tenant_id = ?`,
		`DELETE FROM approval_triage_events WHERE tenant_id = ?`,
		`DELETE FROM clarify_requests WHERE tenant_id = ?`,
		`DELETE FROM notifications WHERE tenant_id = ? AND thread_id IN (SELECT id FROM reset_thread_ids)`,
		`DELETE FROM outbound_messages WHERE tenant_id = ? AND (thread_id IN (SELECT id FROM reset_thread_ids) OR run_id IN (SELECT id FROM reset_run_ids))`,
		`DELETE FROM task_queue WHERE tenant_id = ?`,
		`DELETE FROM effect_receipts WHERE tenant_id = ?`,
		`DELETE FROM maintenance_attempts WHERE tenant_id = ?`,
		`DELETE FROM maintenance_jobs WHERE tenant_id = ?`,
		`DELETE FROM maintenance_provider_calls WHERE tenant_id = ?`,
		`DELETE FROM external_watch_groups WHERE tenant_id = ?`,
		`DELETE FROM external_watches WHERE tenant_id = ?`,
		`DELETE FROM steering_mailbox WHERE tenant_id = ?`,
		`DELETE FROM loop_checkpoints WHERE tenant_id = ?`,
		`DELETE FROM tool_ledger WHERE tenant_id = ?`,
		`DELETE FROM workflow_profiles WHERE tenant_id = ?`,
		`DELETE FROM evolution_candidates WHERE tenant_id = ?`,
		`DELETE FROM run_work_units WHERE identity_tenant_id = ?`,
		`DELETE FROM run_skill_activations WHERE identity_tenant_id = ?`,
		`DELETE FROM skill_candidate_refs WHERE identity_tenant_id = ?`,
		`DELETE FROM workflow_observations WHERE identity_tenant_id = ?`,
		// Published Skill packages stay; only their links into removed learning
		// evidence are detached. A guard keeps protecting the step it recorded
		// without naming a Run that no longer exists, and every citation list —
		// on a frozen evidence snapshot and on a published package's provenance
		// — is filtered down to the observations that still exist. Filtering
		// rather than clearing keeps citations of another tenant's surviving
		// observations, and leaves no id pointing into deleted history.
		`UPDATE skill_failure_guards SET source_run_id = '' WHERE source_run_id IN (SELECT id FROM reset_run_ids)`,
		`UPDATE skill_candidate_evidence_snapshots
			SET observation_ids_json = COALESCE((
				SELECT json_group_array(kept.value)
				  FROM json_each(skill_candidate_evidence_snapshots.observation_ids_json) AS kept
				 WHERE kept.value NOT IN (SELECT id FROM reset_observation_ids)), '[]')
			WHERE json_valid(observation_ids_json) AND json_type(observation_ids_json) = 'array'
			  AND EXISTS (
				SELECT 1 FROM json_each(skill_candidate_evidence_snapshots.observation_ids_json) AS cited
				 WHERE cited.value IN (SELECT id FROM reset_observation_ids))`,
		`UPDATE skill_versions
			SET source_observation_ids_json = COALESCE((
				SELECT json_group_array(kept.value)
				  FROM json_each(skill_versions.source_observation_ids_json) AS kept
				 WHERE kept.value NOT IN (SELECT id FROM reset_observation_ids)), '[]')
			WHERE json_valid(source_observation_ids_json) AND json_type(source_observation_ids_json) = 'array'
			  AND EXISTS (
				SELECT 1 FROM json_each(skill_versions.source_observation_ids_json) AS cited
				 WHERE cited.value IN (SELECT id FROM reset_observation_ids))`,
		`DELETE FROM skill_attributions WHERE run_id IN (SELECT id FROM reset_run_ids)`,
		`DELETE FROM task_skill_bindings WHERE identity_tenant_id = ?`,
		`DELETE FROM task_references WHERE tenant_id = ?`,
		`DELETE FROM runs WHERE tenant_id = ?`,
		`DELETE FROM threads WHERE tenant_id = ?`,
		`DROP TABLE reset_observation_ids`,
		`DROP TABLE reset_reference_ids`,
		`DROP TABLE reset_run_ids`,
		`DROP TABLE reset_thread_ids`,
	} {
		args := []any{}
		if strings.Contains(statement, "?") {
			args = append(args, tenantID)
		}
		if _, err := tx.ExecContext(ctx, statement, args...); err != nil {
			return preview, fmt.Errorf("reset work history: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return preview, err
	}
	return preview, nil
}
