package control

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

const DefaultSkillVerificationMaxAge = 30 * 24 * time.Hour

type SkillStalenessNomination struct {
	ControlTenantID       string    `json:"control_tenant_id"`
	SkillKey              string    `json:"skill_key"`
	SkillName             string    `json:"skill_name"`
	VersionHash           string    `json:"version_hash"`
	Reason                string    `json:"reason"`
	DependencyFingerprint string    `json:"dependency_fingerprint,omitempty"`
	LastVerifiedAt        time.Time `json:"last_verified_at,omitempty"`
}

// SkillVersionReviewReason deterministically nominates review. A nomination
// never authorizes a patch or a model call; publication still goes through the
// normal evidence gates.
func SkillVersionReviewReason(version SkillVersion, currentDependencyFingerprint string, now time.Time, maxAge time.Duration) string {
	if version.State != "active" || version.CreatedBy != "skill_curator" {
		return ""
	}
	if maxAge <= 0 {
		maxAge = DefaultSkillVerificationMaxAge
	}
	currentDependencyFingerprint = strings.TrimSpace(currentDependencyFingerprint)
	if currentDependencyFingerprint != "" && strings.TrimSpace(version.DependencyFingerprint) != "" &&
		currentDependencyFingerprint != version.DependencyFingerprint {
		return "dependency_fingerprint_changed"
	}
	if version.LastVerifiedAt == nil || version.LastVerifiedAt.IsZero() {
		return "never_verified"
	}
	if now.Sub(*version.LastVerifiedAt) >= maxAge {
		return "verification_expired"
	}
	return ""
}

func (s *Store) ListSkillStalenessNominations(ctx context.Context, tenantID, currentDependencyFingerprint string, now time.Time, maxAge time.Duration, limit int) ([]SkillStalenessNomination, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	versions, err := s.ListSkillVersions(ctx, tenantID, "", "active", 100)
	if err != nil {
		return nil, err
	}
	out := make([]SkillStalenessNomination, 0, limit)
	for _, version := range versions {
		reason := SkillVersionReviewReason(version, currentDependencyFingerprint, now, maxAge)
		if reason == "" {
			continue
		}
		nomination := SkillStalenessNomination{
			ControlTenantID: version.ControlTenantID, SkillKey: version.SkillKey,
			SkillName: version.SkillName, VersionHash: version.VersionHash, Reason: reason,
			DependencyFingerprint: version.DependencyFingerprint,
		}
		if version.LastVerifiedAt != nil {
			nomination.LastVerifiedAt = *version.LastVerifiedAt
		}
		out = append(out, nomination)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// EligiblePreviousSkillVersionForEnvironment is the rollback safety check. It
// does not perform rollback: callers still need explicit lifecycle authority.
func (s *Store) EligiblePreviousSkillVersionForEnvironment(ctx context.Context, tenantID, skillKey, failedVersionHash, environmentFingerprint string) (*SkillVersion, error) {
	failed, err := s.GetSkillVersion(ctx, tenantID, skillKey, failedVersionHash)
	if err != nil || failed == nil || strings.TrimSpace(failed.ParentVersionHash) == "" {
		return nil, err
	}
	previous, err := s.GetSkillVersion(ctx, tenantID, skillKey, failed.ParentVersionHash)
	if err != nil || previous == nil {
		return nil, err
	}
	if previous.State != "previous" || previous.LastVerifiedAt == nil || previous.LastVerifiedAt.IsZero() {
		return nil, nil
	}
	environmentFingerprint = strings.TrimSpace(environmentFingerprint)
	if environmentFingerprint == "" || previous.VerificationEnvironmentFingerprint == "" ||
		previous.VerificationEnvironmentFingerprint != environmentFingerprint {
		return nil, nil
	}
	return previous, nil
}

// SkillVersionActivationBlocked exposes only the state needed by discovery
// surfaces to omit an exact quarantined package.
func (s *Store) SkillVersionActivationBlocked(ctx context.Context, tenantID, skillKey, versionHash string) (bool, error) {
	if s == nil || s.db == nil {
		return false, nil
	}
	var state string
	err := s.db.QueryRowContext(ctx, `SELECT state FROM skill_versions
		WHERE control_tenant_id=? AND skill_key=? AND version_hash=?`, normalizeTenant(tenantID), skillKey, versionHash).Scan(&state)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return state == "quarantined", nil
}
