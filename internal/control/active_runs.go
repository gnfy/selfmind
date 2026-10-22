package control

import (
	"context"
	"database/sql"
)

// Execution capacity is independent of Attention: a durably parked or finished
// turn retains its outcome and pending controls without occupying a Run slot.
// Both person capacity and the foreground channel lane use this same predicate.
const activeRunScopeSQL = `tenant_id = ? AND person_id = ? AND status = 'running' AND execution_class = ?`

type activeRunReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func countActiveRuns(ctx context.Context, db activeRunReader, tenant, person, class, channel string) (int, error) {
	query := `SELECT COUNT(*) FROM runs WHERE ` + activeRunScopeSQL
	args := []any{tenant, person, class}
	if channel != "" {
		query += ` AND channel = ?`
		args = append(args, channel)
	}
	var count int
	err := db.QueryRowContext(ctx, query, args...).Scan(&count)
	return count, err
}
