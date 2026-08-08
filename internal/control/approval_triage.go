package control

import (
	"context"
	"database/sql"
	"time"
)

// ApprovalTriageStats is the durable aggregate used by /diag. It contains no
// command text or arguments: only the decision category and a bounded,
// already-redacted provider error are persisted.
type ApprovalTriageStats struct {
	Counts      map[string]int
	LastError   string
	LastErrorAt time.Time
}

type ApprovalTriageEvent struct {
	TenantID, PersonID, TaskID, RunID, ToolName string
	Outcome, RiskLevel, UserAuthorization       string
	GrantKey, ProviderRoute, ErrorClass         string
	PolicyVersion, Rationale, LastError         string
	LatencyMS                                   int64
	At                                          time.Time
}

func (s *Store) RecordApprovalTriageEvent(ctx context.Context, tenantID, personID, outcome, lastError string, at time.Time) error {
	return s.RecordApprovalTriageAudit(ctx, ApprovalTriageEvent{
		TenantID: tenantID, PersonID: personID, Outcome: outcome, LastError: lastError, At: at,
	})
}

func (s *Store) RecordApprovalTriageAudit(ctx context.Context, event ApprovalTriageEvent) error {
	tenantID, personID, outcome, lastError, at := event.TenantID, event.PersonID, event.Outcome, event.LastError, event.At
	if s == nil || personID == "" || outcome == "" {
		return nil
	}
	if len(lastError) > 200 {
		lastError = lastError[:200]
	}
	if at.IsZero() {
		at = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO approval_triage_events
		 (tenant_id, person_id, task_id, run_id, tool_name, outcome, risk_level, user_authorization,
		  grant_key, provider_route, latency_ms, error_class, policy_version, rationale, error, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		normalizeTenant(tenantID), personID, event.TaskID, event.RunID, event.ToolName, outcome,
		event.RiskLevel, event.UserAuthorization, event.GrantKey, event.ProviderRoute, event.LatencyMS,
		event.ErrorClass, event.PolicyVersion, event.Rationale, lastError, at.Unix())
	return err
}

func (s *Store) ApprovalTriageStatsSince(ctx context.Context, tenantID, personID string, since time.Time) (ApprovalTriageStats, error) {
	stats := ApprovalTriageStats{Counts: make(map[string]int)}
	rows, err := s.db.QueryContext(ctx,
		`SELECT outcome, COUNT(*) FROM approval_triage_events
		 WHERE tenant_id = ? AND person_id = ? AND created_at >= ?
		 GROUP BY outcome`,
		normalizeTenant(tenantID), personID, since.Unix())
	if err != nil {
		return stats, err
	}
	for rows.Next() {
		var outcome string
		var count int
		if err := rows.Scan(&outcome, &count); err != nil {
			_ = rows.Close()
			return stats, err
		}
		stats.Counts[outcome] = count
	}
	if err := rows.Close(); err != nil {
		return stats, err
	}
	var at int64
	err = s.db.QueryRowContext(ctx,
		`SELECT error, created_at FROM approval_triage_events
		 WHERE tenant_id = ? AND person_id = ? AND created_at >= ? AND TRIM(error) <> ''
		 ORDER BY created_at DESC, id DESC LIMIT 1`,
		normalizeTenant(tenantID), personID, since.Unix()).Scan(&stats.LastError, &at)
	if err != nil && err != sql.ErrNoRows {
		return stats, err
	}
	if err == nil {
		stats.LastErrorAt = time.Unix(at, 0)
	}
	return stats, nil
}

func (s *Store) PruneApprovalTriageEvents(ctx context.Context, olderThan time.Time) (int64, error) {
	if s == nil || olderThan.IsZero() {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM approval_triage_events WHERE created_at < ?`, olderThan.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
