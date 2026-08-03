package executionenv

import (
	"strings"
	"time"
)

const BindingVersion = 1

const (
	CapabilitySourceTrust        = "workspace_trust"
	CapabilitySourceGrant        = "durable_grant"
	CapabilitySourceRegistration = "watch_registration"
)

// CapabilityBinding explains why one frozen capability was available. A
// persisted grant remains revocable; a workspace-trust capability disappears
// when trust is withdrawn; a one-shot registration approval is bounded by the
// durable action's own expiry and can be stopped by cancelling that action.
type CapabilityBinding struct {
	Capability          string    `json:"capability"`
	Source              string    `json:"source"`
	GrantID             string    `json:"grant_id,omitempty"`
	ResourceFingerprint string    `json:"resource_fingerprint,omitempty"`
	ExpiresAt           time.Time `json:"expires_at,omitempty"`
}

// Binding is the secret-free execution contract carried by durable work.
//
// A Lease belongs to one foreground run. A Binding is the portable subset a
// watcher (and later a remote runner request) needs after that run has ended:
// which environment generation, identity, trust decision, and capabilities the
// user actually approved. Secret values remain in the process-local Snapshot.
type Binding struct {
	Version               int                 `json:"version"`
	ID                    string              `json:"id"`
	SourceLeaseID         string              `json:"source_lease_id,omitempty"`
	TenantID              string              `json:"tenant_id"`
	PersonID              string              `json:"person_id"`
	WorkspaceID           string              `json:"workspace_id,omitempty"`
	EnvironmentProfile    string              `json:"environment_profile,omitempty"`
	CredentialRefs        []CredentialRef     `json:"credential_refs,omitempty"`
	PrincipalFingerprint  string              `json:"principal_fingerprint,omitempty"`
	ExecutionCapabilities []string            `json:"execution_capabilities,omitempty"`
	CapabilityBindings    []CapabilityBinding `json:"capability_bindings,omitempty"`
	TrustLevel            string              `json:"trust_level,omitempty"`

	EnvironmentSnapshotID  string `json:"environment_snapshot_id,omitempty"`
	EnvironmentGeneration  int64  `json:"environment_generation,omitempty"`
	EnvironmentFingerprint string `json:"environment_fingerprint,omitempty"`
	CredentialSourceHash   string `json:"credential_source_hash,omitempty"`
}

// BindingFromLease freezes the non-secret execution material a durable action
// inherited from its creating run. The caller supplies the effective
// capabilities after middleware approval, because those can be broader than
// the lease's start-of-run snapshot.
func BindingFromLease(id string, lease Lease, trustLevel string, capabilities []string, snapshot *Snapshot) Binding {
	binding := Binding{
		Version:                BindingVersion,
		ID:                     strings.TrimSpace(id),
		SourceLeaseID:          strings.TrimSpace(lease.ID),
		TenantID:               strings.TrimSpace(lease.TenantID),
		PersonID:               strings.TrimSpace(lease.PersonID),
		WorkspaceID:            strings.TrimSpace(lease.WorkspaceID),
		EnvironmentProfile:     strings.TrimSpace(lease.EnvironmentProfile),
		CredentialRefs:         append([]CredentialRef{}, lease.CredentialRefs...),
		PrincipalFingerprint:   strings.TrimSpace(lease.PrincipalFingerprint),
		ExecutionCapabilities:  uniqueCapabilities(capabilities),
		TrustLevel:             strings.TrimSpace(trustLevel),
		EnvironmentSnapshotID:  strings.TrimSpace(lease.EnvironmentSnapshotID),
		EnvironmentGeneration:  lease.EnvironmentGeneration,
		EnvironmentFingerprint: strings.TrimSpace(lease.EnvironmentFingerprint),
		CredentialSourceHash:   strings.TrimSpace(lease.CredentialSourceHash),
	}
	if snapshot != nil {
		binding.EnvironmentSnapshotID = snapshot.ID
		binding.EnvironmentGeneration = snapshot.Generation
		binding.PrincipalFingerprint = snapshot.PrincipalFingerprint
		binding.EnvironmentFingerprint = snapshot.EnvironmentFingerprint
		binding.CredentialSourceHash = snapshot.CredentialSourceHash
	}
	return binding
}

// HasRecordedIdentity distinguishes new bindings from legacy durable rows.
func (b Binding) HasRecordedIdentity() bool {
	return strings.TrimSpace(b.EnvironmentSnapshotID) != "" ||
		strings.TrimSpace(b.PrincipalFingerprint) != "" ||
		strings.TrimSpace(b.EnvironmentFingerprint) != "" ||
		strings.TrimSpace(b.CredentialSourceHash) != ""
}

// ResolveBinding returns the exact process-local snapshot selected at
// registration. After a daemon restart the old snapshot no longer exists, so a
// compatible current snapshot may be rebound by fingerprint. A different
// account, toolchain, or credential source is never adopted silently.
func ResolveBinding(registry *Registry, binding Binding) (*Snapshot, error) {
	if registry == nil {
		return nil, &EnvironmentChangedError{LeaseID: binding.ID, Changed: []string{"environment unavailable"}}
	}
	if snapshot, ok := registry.ForLease(binding.ID); ok {
		if changed := DescribeEnvironmentChange(bindingAsLease(binding), snapshot); len(changed) == 0 {
			return snapshot, nil
		}
	}
	if snapshot, ok := registry.Get(binding.EnvironmentSnapshotID); ok {
		// Snapshot ids are process-local indices, not authorization material.
		// After a restart generation numbers begin again, so an id can collide
		// with a different principal that happens to share PATH/HOME.
		if changed := DescribeEnvironmentChange(bindingAsLease(binding), snapshot); len(changed) == 0 {
			registry.BindLease(binding.ID, snapshot.ID)
			return snapshot, nil
		}
	}
	current := registry.Current()
	if !binding.HasRecordedIdentity() {
		return current, nil
	}
	if current == nil {
		return nil, &EnvironmentChangedError{LeaseID: binding.ID, Changed: []string{"environment unavailable"}}
	}
	lease := bindingAsLease(binding)
	if changed := DescribeEnvironmentChange(lease, current); len(changed) > 0 {
		return nil, &EnvironmentChangedError{LeaseID: binding.ID, Changed: changed}
	}
	registry.BindLease(binding.ID, current.ID)
	return current, nil
}

func bindingAsLease(binding Binding) *Lease {
	return &Lease{
		ID:                     binding.ID,
		PrincipalFingerprint:   binding.PrincipalFingerprint,
		EnvironmentFingerprint: binding.EnvironmentFingerprint,
		CredentialSourceHash:   binding.CredentialSourceHash,
	}
}

func uniqueCapabilities(values []string) []string {
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
