package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

type TaskBlocker struct {
	ID              string          `json:"id"`
	TenantID        string          `json:"tenant_id"`
	PersonID        string          `json:"person_id"`
	TaskID          string          `json:"task_id"`
	OriginRunID     string          `json:"origin_run_id,omitempty"`
	Kind            string          `json:"kind"`
	Status          string          `json:"status"`
	Detail          json.RawMessage `json:"detail_json,omitempty"`
	ResolvedByRunID string          `json:"resolved_by_run_id,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	ResolvedAt      *time.Time      `json:"resolved_at,omitempty"`
}

func (s *Store) ListOpenTaskBlockers(ctx context.Context, tenantID, taskID string, limit int) ([]TaskBlocker, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, tenant_id, person_id, task_id,
		origin_run_id, kind, status, detail_json, resolved_by_run_id, created_at, resolved_at
		FROM task_blockers WHERE tenant_id = ? AND task_id = ? AND status = 'open'
		ORDER BY created_at ASC, id ASC LIMIT ?`, normalizeTenant(tenantID), strings.TrimSpace(taskID), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var blockers []TaskBlocker
	for rows.Next() {
		var blocker TaskBlocker
		var detail string
		var created int64
		var resolved sql.NullInt64
		if err := rows.Scan(&blocker.ID, &blocker.TenantID, &blocker.PersonID, &blocker.TaskID,
			&blocker.OriginRunID, &blocker.Kind, &blocker.Status, &detail,
			&blocker.ResolvedByRunID, &created, &resolved); err != nil {
			return nil, err
		}
		blocker.Detail = json.RawMessage(detail)
		blocker.CreatedAt = time.Unix(created, 0)
		if resolved.Valid {
			at := time.Unix(resolved.Int64, 0)
			blocker.ResolvedAt = &at
		}
		blockers = append(blockers, blocker)
	}
	return blockers, rows.Err()
}

func ensureRunBlockerTx(ctx context.Context, tx *sql.Tx, tenantID, personID, taskID, runID, proposed, summary string, nextSteps []string, now time.Time) error {
	kind := blockerKindForRunStatus(proposed)
	if kind == "" {
		return nil
	}
	detail, _ := json.Marshal(map[string]interface{}{
		"summary": summary, "next_steps": nextSteps, "run_status": proposed,
	})
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO task_blockers
		(id, tenant_id, person_id, task_id, origin_run_id, kind, status, detail_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 'open', ?, ?)`,
		"blocker_"+runID+"_"+kind, tenantID, personID, taskID, runID, kind, string(detail), now.Unix())
	return err
}

func blockerKindForRunStatus(status string) string {
	switch strings.TrimSpace(status) {
	case "waiting_user":
		return "waiting_user"
	case "blocked":
		return "blocked"
	case "verification_partial":
		return "verification_partial"
	case "interrupted":
		return "run_interrupted"
	default:
		return ""
	}
}

func resolveTaskBlockersTx(ctx context.Context, tx *sql.Tx, tenantID, taskID, runID string, blockerIDs []string, now time.Time) error {
	for _, blockerID := range blockerIDs {
		blockerID = strings.TrimSpace(blockerID)
		if blockerID == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE task_blockers
			SET status = 'resolved', resolved_by_run_id = ?, resolved_at = ?
			WHERE id = ? AND tenant_id = ? AND task_id = ? AND status = 'open'`,
			runID, now.Unix(), blockerID, tenantID, taskID); err != nil {
			return err
		}
	}
	return nil
}

func resolveOriginRunBlockersTx(ctx context.Context, tx *sql.Tx, tenantID, taskID, originRunID, resolvedByRunID string) error {
	_, err := tx.ExecContext(ctx, `UPDATE task_blockers
		SET status = 'resolved', resolved_by_run_id = ?, resolved_at = ?
		WHERE tenant_id = ? AND task_id = ? AND origin_run_id = ? AND status = 'open'`,
		resolvedByRunID, time.Now().Unix(), tenantID, taskID, originRunID)
	return err
}

func openTaskBlockerStatusTx(ctx context.Context, tx *sql.Tx, tenantID, taskID string) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT kind FROM task_blockers
		WHERE tenant_id = ? AND task_id = ? AND status = 'open'`, tenantID, taskID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			return "", err
		}
		seen[kind] = true
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	for _, item := range []struct{ kind, status string }{
		{"waiting_user", "waiting_user"},
		{"verification_partial", "verification_partial"},
		{"blocked", "blocked"},
		{"run_interrupted", "interrupted"},
	} {
		if seen[item.kind] {
			return item.status, nil
		}
	}
	return "", nil
}
