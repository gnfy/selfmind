package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"selfmind/internal/executionenv"
)

// MaterializeExecutionLease creates the immutable environment binding for a
// run. Replaying the same run returns the existing lease rather than taking a
// fresh snapshot halfway through an execution.
func (s *Store) MaterializeExecutionLease(ctx context.Context, lease executionenv.Lease) (*executionenv.Lease, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("control store is unavailable")
	}
	if strings.TrimSpace(lease.RunID) == "" || strings.TrimSpace(lease.PersonID) == "" {
		return nil, fmt.Errorf("run id and person id are required")
	}
	if existing, err := s.GetExecutionLeaseByRun(ctx, lease.TenantID, lease.RunID); err == nil && existing != nil {
		return existing, nil
	} else if err != nil {
		return nil, err
	}
	if lease.ID == "" {
		lease.ID = "lease_" + uuid.NewString()
	}
	lease.TenantID = normalizeTenant(lease.TenantID)
	if lease.EnvironmentProfile == "" {
		lease.EnvironmentProfile = "operator"
	}
	now := time.Now()
	lease.CreatedAt = now
	lease.UpdatedAt = now
	refs, _ := json.Marshal(lease.CredentialRefs)
	caps, _ := json.Marshal(uniqueStrings(lease.ExecutionCapabilities))
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO execution_leases
		(id, run_id, tenant_id, person_id, workspace_id, environment_profile,
		 credential_refs_json, principal_fingerprint, capabilities_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		lease.ID, lease.RunID, lease.TenantID, lease.PersonID, lease.WorkspaceID,
		lease.EnvironmentProfile, string(refs), lease.PrincipalFingerprint, string(caps),
		now.Unix(), now.Unix())
	if err != nil {
		return nil, err
	}
	materialized, err := s.GetExecutionLeaseByRun(ctx, lease.TenantID, lease.RunID)
	if err != nil {
		return nil, err
	}
	if materialized == nil {
		return nil, fmt.Errorf("execution lease was not materialized for run %s; a conflicting tenant or run binding may exist", lease.RunID)
	}
	return materialized, nil
}

func (s *Store) GetExecutionLeaseByRun(ctx context.Context, tenantID, runID string) (*executionenv.Lease, error) {
	var lease executionenv.Lease
	var refsJSON, capsJSON string
	var created, updated int64
	err := s.db.QueryRowContext(ctx, `SELECT id, run_id, tenant_id, person_id, COALESCE(workspace_id, ''),
		environment_profile, credential_refs_json, COALESCE(principal_fingerprint, ''),
		capabilities_json, created_at, updated_at
		FROM execution_leases WHERE tenant_id = ? AND run_id = ?`,
		normalizeTenant(tenantID), runID).Scan(
		&lease.ID, &lease.RunID, &lease.TenantID, &lease.PersonID, &lease.WorkspaceID,
		&lease.EnvironmentProfile, &refsJSON, &lease.PrincipalFingerprint,
		&capsJSON, &created, &updated)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(refsJSON), &lease.CredentialRefs)
	_ = json.Unmarshal([]byte(capsJSON), &lease.ExecutionCapabilities)
	lease.CreatedAt = time.Unix(created, 0)
	lease.UpdatedAt = time.Unix(updated, 0)
	return &lease, nil
}

func (s *Store) GrantExecutionCapability(
	ctx context.Context,
	tenantID, personID, workspaceID, capability, resourceFingerprint, grantedBy string,
	expiresAt time.Time,
) error {
	if workspaceID == "" || capability == "" {
		return fmt.Errorf("workspace id and capability are required")
	}
	if expiresAt.IsZero() || !expiresAt.After(time.Now()) {
		return fmt.Errorf("capability expiry must be in the future")
	}
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `INSERT INTO execution_capability_grants
		(id, tenant_id, person_id, workspace_id, capability, resource_fingerprint,
		 granted_by, expires_at, revoked_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)
		ON CONFLICT(tenant_id, person_id, workspace_id, capability, resource_fingerprint)
		DO UPDATE SET granted_by = excluded.granted_by, expires_at = excluded.expires_at,
		 revoked_at = NULL, updated_at = excluded.updated_at`,
		"cap_"+uuid.NewString(), normalizeTenant(tenantID), personID, workspaceID,
		capability, strings.TrimSpace(resourceFingerprint), strings.TrimSpace(grantedBy),
		expiresAt.Unix(), now, now)
	return err
}

func (s *Store) HasExecutionCapability(
	ctx context.Context,
	tenantID, personID, workspaceID, capability, resourceFingerprint string,
) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_capability_grants
		WHERE tenant_id = ? AND person_id = ? AND workspace_id = ? AND capability = ?
		  AND resource_fingerprint = ? AND revoked_at IS NULL AND expires_at > ?`,
		normalizeTenant(tenantID), personID, workspaceID, capability,
		strings.TrimSpace(resourceFingerprint), time.Now().Unix()).Scan(&count)
	return count > 0, err
}

func (s *Store) ListActiveExecutionCapabilities(
	ctx context.Context, tenantID, personID, workspaceID string,
) ([]executionenv.CapabilityGrant, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, tenant_id, person_id, workspace_id,
		capability, resource_fingerprint, COALESCE(granted_by, ''), expires_at,
		revoked_at, created_at, updated_at
		FROM execution_capability_grants
		WHERE tenant_id = ? AND person_id = ? AND workspace_id = ?
		  AND revoked_at IS NULL AND expires_at > ?
		ORDER BY expires_at ASC`,
		normalizeTenant(tenantID), personID, workspaceID, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []executionenv.CapabilityGrant
	for rows.Next() {
		var grant executionenv.CapabilityGrant
		var revoked sql.NullInt64
		var expires, created, updated int64
		if err := rows.Scan(&grant.ID, &grant.TenantID, &grant.PersonID, &grant.WorkspaceID,
			&grant.Capability, &grant.ResourceFingerprint, &grant.GrantedBy, &expires,
			&revoked, &created, &updated); err != nil {
			return nil, err
		}
		grant.ExpiresAt = time.Unix(expires, 0)
		grant.CreatedAt = time.Unix(created, 0)
		grant.UpdatedAt = time.Unix(updated, 0)
		if revoked.Valid {
			value := time.Unix(revoked.Int64, 0)
			grant.RevokedAt = &value
		}
		out = append(out, grant)
	}
	return out, rows.Err()
}

func (s *Store) RevokeExecutionCapability(
	ctx context.Context, tenantID, personID, workspaceID, capability string,
) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `UPDATE execution_capability_grants
		SET revoked_at = ?, updated_at = ?
		WHERE tenant_id = ? AND person_id = ? AND workspace_id = ? AND capability = ?
		  AND revoked_at IS NULL`,
		now, now, normalizeTenant(tenantID), personID, workspaceID, capability)
	return err
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
