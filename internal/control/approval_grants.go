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

// Approval grants are the durable backing for class-level approval memory. A
// grant says "this action CLASS (pattern_key) is pre-approved" for a scope, and
// "person" is the only durable scope: scope_id is a person id and the grant
// applies to all of that person's work. Person plus pattern_key IS the
// category-scoped grant — it needs no container.
//
// A "task" scope used to exist as session memory, surviving across runs of one
// task. It was removed because Task never earned that authority: grant reuse
// depended on the judgment that a set of runs is one piece of work, and that
// judgment demonstrably mis-groups unrelated runs, so one decision could be
// inherited by work the person never saw. The server-issued option set had
// already stopped offering it (buildApprovalDecisions offers once / run-local /
// deny), which made every remaining task-scoped row unreachable.
//
// Transient run-local reuse is not here at all: it lives in the execution
// scope and dies with the run.
//
// Content-level and hard-floor denials are never eligible: the middleware only
// reaches the grant path after the hard floor, and only records a grant when a
// human decision explicitly asks to remember. Grants live in control.db so they
// are visible to every surface, exactly like the rest of durable work state.
//
// A grant is user-owned state, so it must be listable, expirable and
// revocable. Earlier this table supported only INSERT plus an existence check,
// which meant a remembered class could never be reviewed or withdrawn — a
// defective class key became a permanent standing permission.

// ApprovalGrant is one remembered approval class.
type ApprovalGrant struct {
	ID         string    `json:"id"`
	TenantID   string    `json:"tenant_id"`
	PersonID   string    `json:"person_id"`
	ScopeKind  string    `json:"scope_kind"`
	ScopeID    string    `json:"scope_id"`
	PatternKey string    `json:"pattern_key"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at,omitempty"`
	RevokedAt  time.Time `json:"revoked_at,omitempty"`
}

// Expired reports whether the grant carries a deadline that has passed. A zero
// deadline is the legacy "no expiry" meaning and never expires on its own.
func (g ApprovalGrant) Expired(now time.Time) bool {
	return !g.ExpiresAt.IsZero() && !g.ExpiresAt.After(now)
}

// Revoked reports whether the grant was explicitly withdrawn.
func (g ApprovalGrant) Revoked() bool { return !g.RevokedAt.IsZero() }

// Active reports whether the grant can still authorize its class.
func (g ApprovalGrant) Active(now time.Time) bool { return !g.Revoked() && !g.Expired(now) }

// GrantApproval records (or refreshes) a class-level approval grant. scopeKind
// must be "person" (scopeID = person id). expiresAt bounds the grant; a zero
// value means no deadline. Re-granting an existing class refreshes both
// timestamps and clears a previous revocation, because the caller has just made
// a fresh human decision.
func (s *Store) GrantApproval(ctx context.Context, scopeKind, tenantID, personID, scopeID, patternKey string, expiresAt time.Time) error {
	scopeKind = normalizeGrantScope(scopeKind)
	if scopeKind != "person" {
		return fmt.Errorf("grant scope must be person")
	}
	tenantID = normalizeTenant(tenantID)
	personID = strings.TrimSpace(personID)
	scopeID = strings.TrimSpace(scopeID)
	patternKey = strings.TrimSpace(patternKey)
	if personID == "" || scopeID == "" || patternKey == "" {
		return fmt.Errorf("person id, scope id and pattern key are required")
	}
	expiry := int64(0)
	if !expiresAt.IsZero() {
		expiry = expiresAt.Unix()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO approval_grants (id, tenant_id, person_id, scope_kind, scope_id, pattern_key, created_at, expires_at, revoked_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0)
		 ON CONFLICT(tenant_id, person_id, scope_kind, scope_id, pattern_key)
		 DO UPDATE SET created_at = excluded.created_at,
		               expires_at = excluded.expires_at,
		               revoked_at = 0`,
		"agr_"+uuid.NewString(), tenantID, personID, scopeKind, scopeID, patternKey,
		time.Now().Unix(), expiry)
	return err
}

// IsApprovalGranted reports whether patternKey is already approved for the
// person. Expired and revoked grants never match.
func (s *Store) IsApprovalGranted(ctx context.Context, tenantID, personID, patternKey string) (bool, error) {
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
		   AND revoked_at = 0
		   AND (expires_at = 0 OR expires_at > ?)
		   AND scope_kind = 'person' AND scope_id = ?
		 LIMIT 1`,
		tenantID, personID, patternKey, time.Now().Unix(), personID).Scan(&one)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ListApprovalGrants returns the person's remembered classes, newest first.
// includeInactive keeps expired and revoked rows so a review surface can show
// what was withdrawn instead of silently hiding history.
func (s *Store) ListApprovalGrants(ctx context.Context, tenantID, personID string, includeInactive bool) ([]ApprovalGrant, error) {
	tenantID = normalizeTenant(tenantID)
	personID = strings.TrimSpace(personID)
	if personID == "" {
		return nil, nil
	}
	query := `SELECT id, tenant_id, person_id, scope_kind, scope_id, pattern_key, created_at, expires_at, revoked_at
	          FROM approval_grants WHERE tenant_id = ? AND person_id = ?`
	args := []interface{}{tenantID, personID}
	if !includeInactive {
		query += ` AND revoked_at = 0 AND (expires_at = 0 OR expires_at > ?)`
		args = append(args, time.Now().Unix())
	}
	query += ` ORDER BY created_at DESC, id DESC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	grants := make([]ApprovalGrant, 0)
	for rows.Next() {
		var grant ApprovalGrant
		var createdAt, expiresAt, revokedAt int64
		if err := rows.Scan(&grant.ID, &grant.TenantID, &grant.PersonID, &grant.ScopeKind,
			&grant.ScopeID, &grant.PatternKey, &createdAt, &expiresAt, &revokedAt); err != nil {
			return nil, err
		}
		grant.CreatedAt = time.Unix(createdAt, 0)
		if expiresAt > 0 {
			grant.ExpiresAt = time.Unix(expiresAt, 0)
		}
		if revokedAt > 0 {
			grant.RevokedAt = time.Unix(revokedAt, 0)
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

// ListAllApprovalGrants returns every active grant across persons. It backs the
// boot-time review sweep, which must re-check keys minted by an earlier version
// of the eligibility floor; ordinary surfaces use ListApprovalGrants.
func (s *Store) ListAllApprovalGrants(ctx context.Context) ([]ApprovalGrant, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tenant_id, person_id, scope_kind, scope_id, pattern_key, created_at, expires_at, revoked_at
		 FROM approval_grants
		 WHERE revoked_at = 0 AND (expires_at = 0 OR expires_at > ?)
		 ORDER BY created_at DESC, id DESC`, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	grants := make([]ApprovalGrant, 0)
	for rows.Next() {
		var grant ApprovalGrant
		var createdAt, expiresAt, revokedAt int64
		if err := rows.Scan(&grant.ID, &grant.TenantID, &grant.PersonID, &grant.ScopeKind,
			&grant.ScopeID, &grant.PatternKey, &createdAt, &expiresAt, &revokedAt); err != nil {
			return nil, err
		}
		grant.CreatedAt = time.Unix(createdAt, 0)
		if expiresAt > 0 {
			grant.ExpiresAt = time.Unix(expiresAt, 0)
		}
		if revokedAt > 0 {
			grant.RevokedAt = time.Unix(revokedAt, 0)
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

// RevokeApprovalGrant withdraws one grant by id. Revocation is recorded rather
// than deleted so the decision stays auditable. Returns whether a row changed.
func (s *Store) RevokeApprovalGrant(ctx context.Context, tenantID, personID, grantID string) (bool, error) {
	tenantID = normalizeTenant(tenantID)
	personID = strings.TrimSpace(personID)
	grantID = strings.TrimSpace(grantID)
	if personID == "" || grantID == "" {
		return false, nil
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE approval_grants SET revoked_at = ?
		 WHERE tenant_id = ? AND person_id = ? AND id = ? AND revoked_at = 0`,
		time.Now().Unix(), tenantID, personID, grantID)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
}
