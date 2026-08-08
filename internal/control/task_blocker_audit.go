package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// TaskBlockerAuditFinding describes a historical task whose parked status is
// not represented by an open blocker. SafeToApply is deliberately narrow: the
// task must be inactive and its status must exactly match its newest finished
// run. Mixed labels and later successful runs are report-only.
type TaskBlockerAuditFinding struct {
	TaskID          string
	PersonID        string
	Title           string
	TaskStatus      string
	LatestRunID     string
	LatestRunStatus string
	BlockerKind     string
	SafeToApply     bool
	Reason          string
}

// AuditMissingTaskBlockers is an offline governance query. It never mutates
// task state and intentionally includes every person in a tenant so migrations
// do not depend on whichever CLI account happens to invoke the command.
func (s *Store) AuditMissingTaskBlockers(ctx context.Context, tenantID string, limit int) ([]TaskBlockerAuditFinding, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("control store is unavailable")
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, person_id, title, status, COALESCE(active_run_id, '')
		FROM tasks
		WHERE tenant_id = ? AND archived_at IS NULL
		  AND status IN ('waiting_user', 'verification_partial', 'blocked', 'interrupted')
		ORDER BY updated_at DESC, id DESC LIMIT ?`, normalizeTenant(tenantID), limit)
	if err != nil {
		return nil, err
	}
	type candidate struct{ taskID, personID, title, status, activeRunID string }
	var candidates []candidate
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.taskID, &item.personID, &item.title, &item.status, &item.activeRunID); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, item)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	findings := make([]TaskBlockerAuditFinding, 0)
	for _, item := range candidates {
		var openCount int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_blockers
			WHERE tenant_id = ? AND task_id = ? AND status = 'open'`, normalizeTenant(tenantID), item.taskID).Scan(&openCount); err != nil {
			return nil, err
		}
		if openCount > 0 {
			continue
		}
		finding := TaskBlockerAuditFinding{
			TaskID: item.taskID, PersonID: item.personID, Title: item.title, TaskStatus: item.status,
		}
		if item.activeRunID != "" {
			finding.Reason = "task still has an active run"
			findings = append(findings, finding)
			continue
		}
		var finished sql.NullInt64
		err := s.db.QueryRowContext(ctx, `SELECT id, status, finished_at FROM task_runs
			WHERE tenant_id = ? AND task_id = ? ORDER BY started_at DESC, id DESC LIMIT 1`,
			normalizeTenant(tenantID), item.taskID).Scan(&finding.LatestRunID, &finding.LatestRunStatus, &finished)
		if err == sql.ErrNoRows {
			finding.Reason = "task has no run evidence"
			findings = append(findings, finding)
			continue
		}
		if err != nil {
			return nil, err
		}
		finding.BlockerKind = blockerKindForRunStatus(finding.LatestRunStatus)
		switch {
		case !finished.Valid:
			finding.Reason = "latest run is not finished"
		case finding.TaskStatus != finding.LatestRunStatus:
			finding.Reason = "task status conflicts with latest run; label may contain mixed work"
		case finding.BlockerKind == "":
			finding.Reason = "latest run does not encode a blocker status"
		default:
			var prior int
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_blockers
				WHERE tenant_id = ? AND task_id = ? AND origin_run_id = ? AND kind = ?`,
				normalizeTenant(tenantID), item.taskID, finding.LatestRunID, finding.BlockerKind).Scan(&prior); err != nil {
				return nil, err
			}
			if prior > 0 {
				finding.Reason = "matching blocker was already resolved; do not reopen automatically"
			} else {
				finding.SafeToApply = true
				finding.Reason = "task and latest finished run agree"
			}
		}
		findings = append(findings, finding)
	}
	return findings, nil
}

// BackfillTaskBlocker applies one audit finding with an atomic evidence recheck.
// It only creates the missing blocker; it never rewrites task or run status.
func (s *Store) BackfillTaskBlocker(ctx context.Context, tenantID string, finding TaskBlockerAuditFinding) (bool, error) {
	if !finding.SafeToApply || strings.TrimSpace(finding.BlockerKind) == "" {
		return false, nil
	}
	tenant := normalizeTenant(tenantID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var personID, taskStatus, activeRunID string
	if err := tx.QueryRowContext(ctx, `SELECT person_id, status, COALESCE(active_run_id, '')
		FROM tasks WHERE tenant_id = ? AND id = ? AND archived_at IS NULL`, tenant, finding.TaskID).
		Scan(&personID, &taskStatus, &activeRunID); err != nil {
		return false, err
	}
	var runID, runStatus, summary string
	var finished sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT id, status, COALESCE(input_summary, ''), finished_at
		FROM task_runs WHERE tenant_id = ? AND task_id = ? ORDER BY started_at DESC, id DESC LIMIT 1`,
		tenant, finding.TaskID).Scan(&runID, &runStatus, &summary, &finished); err != nil {
		return false, err
	}
	kind := blockerKindForRunStatus(runStatus)
	if activeRunID != "" || !finished.Valid || runID != finding.LatestRunID || taskStatus != runStatus || kind != finding.BlockerKind {
		return false, nil
	}
	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_blockers
		WHERE tenant_id = ? AND task_id = ? AND (status = 'open' OR (origin_run_id = ? AND kind = ?))`,
		tenant, finding.TaskID, runID, kind).Scan(&existing); err != nil {
		return false, err
	}
	if existing > 0 {
		return false, nil
	}
	detail, _ := json.Marshal(map[string]interface{}{
		"summary": summary, "next_steps": []string{}, "run_status": runStatus,
		"source": "maintenance.task-audit",
	})
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO task_blockers
		(id, tenant_id, person_id, task_id, origin_run_id, kind, status, detail_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 'open', ?, ?)`,
		"blocker_"+runID+"_"+kind, tenant, personID, finding.TaskID, runID, kind, string(detail), time.Now().Unix())
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows == 0 {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
