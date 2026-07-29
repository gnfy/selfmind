package executionenv

import "time"

const (
	TrustTrusted   = "trusted"
	TrustUntrusted = "untrusted"

	CapabilityNetworkShared     = "network:shared"
	CapabilityCredentialRead    = "credential:read"
	CapabilityHostEscape        = "host:escape"
	CapabilityCredentialRefresh = "credential:refresh"
)

// CredentialRef identifies a credential source without carrying secret bytes.
// Values remain process-local and are registered only with the runtime
// redactor. Durable state stores this reference and a non-secret principal.
type CredentialRef struct {
	Kind      string `json:"kind"`
	Source    string `json:"source"`
	Principal string `json:"principal,omitempty"`
}

// Lease binds one run to the environment snapshot it started with. A resumed
// task creates a new run and therefore a new lease; retrying the same durable
// run reuses its existing lease.
type Lease struct {
	ID                    string          `json:"id"`
	RunID                 string          `json:"run_id"`
	TenantID              string          `json:"tenant_id"`
	PersonID              string          `json:"person_id"`
	WorkspaceID           string          `json:"workspace_id,omitempty"`
	EnvironmentProfile    string          `json:"environment_profile"`
	CredentialRefs        []CredentialRef `json:"credential_refs,omitempty"`
	PrincipalFingerprint  string          `json:"principal_fingerprint,omitempty"`
	ExecutionCapabilities []string        `json:"execution_capabilities,omitempty"`
	// EnvironmentSnapshotID and EnvironmentGeneration bind the run to one
	// in-process environment snapshot. The lease is the control point, not just
	// an audit record: every command of the run resolves its child environment
	// through this binding instead of re-reading the daemon's own environment.
	EnvironmentSnapshotID string `json:"environment_snapshot_id,omitempty"`
	EnvironmentGeneration int64  `json:"environment_generation,omitempty"`
	// EnvironmentFingerprint and CredentialSourceHash are the non-secret
	// descriptions that let a restarted daemon decide whether rebuilding the
	// snapshot is safe. See Snapshot.Matches.
	EnvironmentFingerprint string    `json:"environment_fingerprint,omitempty"`
	CredentialSourceHash   string    `json:"credential_source_hash,omitempty"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
}

// CapabilityGrant is a time-bounded workspace capability. It deliberately
// scopes grants to a workspace and non-secret resource fingerprint; request
// source and raw command text never become authorization material.
type CapabilityGrant struct {
	ID                  string     `json:"id"`
	TenantID            string     `json:"tenant_id"`
	PersonID            string     `json:"person_id"`
	WorkspaceID         string     `json:"workspace_id"`
	Capability          string     `json:"capability"`
	ResourceFingerprint string     `json:"resource_fingerprint,omitempty"`
	GrantedBy           string     `json:"granted_by,omitempty"`
	ExpiresAt           time.Time  `json:"expires_at"`
	RevokedAt           *time.Time `json:"revoked_at,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}
