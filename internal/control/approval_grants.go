package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Approval grants are the durable backing for class-level approval memory (the
// layered approval funnel's session/persistent allowlist). A grant says "this
// action CLASS (pattern_key) is pre-approved" for a scope:
//
//   - scope_kind "task"   → scope_id is a task id; the grant survives across
//     runs of that task but not other tasks (session memory).
//   - scope_kind "person" → scope_id is a person id; the grant applies to every
//     task the person owns (persistent memory).
//
// Content-level and hard-floor denials are never eligible: the middleware only
// reaches the grant path after the hard floor, and only records a grant when a
// human decision explicitly asks to remember. Grants live in control.db so they
// are visible to every surface, exactly like the rest of durable work state.

// GrantApproval records (or refreshes) a class-level approval grant. scopeKind
// is "task" (scopeID = task id) or "person" (scopeID = person id). Duplicate
// grants are idempotent.
func (s *Store) GrantApproval(ctx context.Context, scopeKind, tenantID, personID, scopeID, patternKey string) error {
	scopeKind = normalizeGrantScope(scopeKind)
	if scopeKind == "" {
		return fmt.Errorf("grant scope must be task or person")
	}
	tenantID = normalizeTenant(tenantID)
	personID = strings.TrimSpace(personID)
	scopeID = strings.TrimSpace(scopeID)
	patternKey = strings.TrimSpace(patternKey)
	if personID == "" || scopeID == "" || patternKey == "" {
		return fmt.Errorf("person id, scope id and pattern key are required")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO approval_grants (id, tenant_id, person_id, scope_kind, scope_id, pattern_key, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(tenant_id, person_id, scope_kind, scope_id, pattern_key)
		 DO UPDATE SET created_at = excluded.created_at`,
		"agr_"+uuid.NewString(), tenantID, personID, scopeKind, scopeID, patternKey, time.Now().Unix())
	return err
}

// IsApprovalGranted reports whether patternKey is already approved for the
// person — either by a person-scoped grant (any task) or a task-scoped grant
// for taskID. taskID may be empty (then only person-scoped grants match).
func (s *Store) IsApprovalGranted(ctx context.Context, tenantID, personID, taskID, patternKey string) (bool, error) {
	tenantID = normalizeTenant(tenantID)
	personID = strings.TrimSpace(personID)
	patternKey = strings.TrimSpace(patternKey)
	if personID == "" || patternKey == "" {
		return false, nil
	}
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM approval_grants
		 WHERE tenant_id = ? AND person_id = ? AND pattern_key = ?
		   AND ( (scope_kind = 'person' AND scope_id = ?)
		      OR (scope_kind = 'task' AND scope_id = ?) )
		 LIMIT 1`,
		tenantID, personID, patternKey, personID, strings.TrimSpace(taskID)).Scan(&one)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
