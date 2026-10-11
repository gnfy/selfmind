package control

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"selfmind/internal/executionenv"
)

// ExecutionViewUsage is the durable evidence view retention compares before
// and after its slow inspection: whether unfinished work still refers to the
// exact view, and every Run that was ever bound to it. A Run that started and
// finished in between changes RunIDs even though it is no longer pending.
type ExecutionViewUsage struct {
	Pending bool
	RunIDs  []string
}

// unfinishedRunStatuses are Run states whose work may still execute, now or
// after a resume, in the roots the Run is bound to.
var unfinishedRunStatuses = []string{"running", "waiting_external", "waiting_user", "blocked", "interrupted", "verification_partial"}

var executionViewPendingRunStatuses = func() map[string]bool {
	statuses := make(map[string]bool, len(unfinishedRunStatuses))
	for _, status := range unfinishedRunStatuses {
		statuses[status] = true
	}
	return statuses
}()

// ExecutionViewUsage reads Run and queue bindings for one exact view path.
// Rows are prefiltered by the view directory name when that name is plain
// enough to appear verbatim in any JSON encoding; the exact path decides.
func (s *Store) ExecutionViewUsage(ctx context.Context, tenantID, personID, path string) (ExecutionViewUsage, error) {
	if s == nil || s.db == nil || personID == "" || !filepath.IsAbs(path) {
		return ExecutionViewUsage{}, fmt.Errorf("exact execution view owner and path are required")
	}
	path = filepath.Clean(path)
	filter, args := "", []interface{}{normalizeTenant(tenantID), personID}
	if base := filepath.Base(path); plainExecutionViewName(base) {
		filter, args = ` AND instr(COALESCE(execution_roots_json,''), ?) > 0`, append(args, base)
	}
	var usage ExecutionViewUsage
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, status, COALESCE(execution_roots_json,'[]') FROM runs WHERE tenant_id=? AND person_id=?`+filter+` ORDER BY id`, args...)
	if err != nil {
		return ExecutionViewUsage{}, err
	}
	for rows.Next() {
		var id, status, raw string
		if err := rows.Scan(&id, &status, &raw); err != nil {
			_ = rows.Close()
			return ExecutionViewUsage{}, err
		}
		bound, err := rootsBindExecutionView(raw, path)
		if err != nil {
			_ = rows.Close()
			return ExecutionViewUsage{}, err
		}
		if bound {
			usage.RunIDs = append(usage.RunIDs, id)
			usage.Pending = usage.Pending || executionViewPendingRunStatuses[status]
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return ExecutionViewUsage{}, err
	}
	_ = rows.Close()
	queued, err := s.db.QueryContext(ctx,
		`SELECT COALESCE(execution_roots_json,'[]') FROM task_queue WHERE tenant_id=? AND person_id=? AND status IN ('queued','started')`+filter, args...)
	if err != nil {
		return ExecutionViewUsage{}, err
	}
	defer queued.Close()
	for queued.Next() {
		var raw string
		if err := queued.Scan(&raw); err != nil {
			return ExecutionViewUsage{}, err
		}
		bound, err := rootsBindExecutionView(raw, path)
		if err != nil {
			return ExecutionViewUsage{}, err
		}
		usage.Pending = usage.Pending || bound
	}
	return usage, queued.Err()
}

// UnfinishedRunRoots is one Run that may still execute in its bound roots:
// running, or parked and able to resume there.
type UnfinishedRunRoots struct {
	RunID  string
	Status string
	Roots  []executionenv.RootBinding
}

// ListUnfinishedRunRoots returns the person's unfinished Runs with their exact
// root bindings, so a caller can tell whether any of them may still write a
// directory the daemon is about to act on.
func (s *Store) ListUnfinishedRunRoots(ctx context.Context, tenantID, personID string) ([]UnfinishedRunRoots, error) {
	if s == nil || s.db == nil || personID == "" {
		return nil, fmt.Errorf("exact person is required")
	}
	args := []interface{}{normalizeTenant(tenantID), personID}
	for _, status := range unfinishedRunStatuses {
		args = append(args, status)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, status, COALESCE(execution_roots_json,'[]') FROM runs
		 WHERE tenant_id=? AND person_id=? AND status IN (?`+strings.Repeat(",?", len(unfinishedRunStatuses)-1)+`)
		 ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UnfinishedRunRoots
	for rows.Next() {
		var run UnfinishedRunRoots
		var raw string
		if err := rows.Scan(&run.RunID, &run.Status, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &run.Roots); err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

func rootsBindExecutionView(raw, path string) (bool, error) {
	var roots []executionenv.RootBinding
	if err := json.Unmarshal([]byte(raw), &roots); err != nil {
		return false, err
	}
	for _, root := range roots {
		if filepath.Clean(root.Path) == path {
			return true, nil
		}
	}
	return false, nil
}

func plainExecutionViewName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, char := range name {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' || char == '.') {
			return false
		}
	}
	return true
}
