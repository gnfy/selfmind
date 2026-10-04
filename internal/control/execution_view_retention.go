package control

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"selfmind/internal/executionenv"
)

// ExecutionViewReferencedByPendingWork checks durable Run and queue bindings
// before a delivered view is retired. A newly admitted execution with a
// missing root still fails closed, but this check avoids disrupting known
// work that has not yet finished or resumed.
func (s *Store) ExecutionViewReferencedByPendingWork(ctx context.Context, tenantID, personID, path string) (bool, error) {
	if s == nil || s.db == nil || personID == "" || !filepath.IsAbs(path) {
		return false, fmt.Errorf("exact execution view owner and path are required")
	}
	queries := []string{
		`SELECT COALESCE(execution_roots_json,'[]') FROM runs WHERE tenant_id=? AND person_id=?
		 AND status IN ('running','waiting_external','waiting_user','blocked','interrupted','verification_partial')`,
		`SELECT COALESCE(execution_roots_json,'[]') FROM task_queue WHERE tenant_id=? AND person_id=?
		 AND status IN ('queued','started')`,
	}
	for _, query := range queries {
		rows, err := s.db.QueryContext(ctx, query, normalizeTenant(tenantID), personID)
		if err != nil {
			return false, err
		}
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				_ = rows.Close()
				return false, err
			}
			var roots []executionenv.RootBinding
			if err := json.Unmarshal([]byte(raw), &roots); err != nil {
				_ = rows.Close()
				return false, err
			}
			for _, root := range roots {
				if filepath.Clean(root.Path) == filepath.Clean(path) {
					_ = rows.Close()
					return true, nil
				}
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return false, err
		}
		_ = rows.Close()
	}
	return false, nil
}
