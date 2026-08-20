package control

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type SkillFallbackInput struct {
	IdentityTenantID     string
	RunID                string
	WorkUnitID           string
	ExecutionLane        string
	Reason               string
	FailureSignature     string
	FailedStepID         string
	ErrorCategory        string
	NormalizedInputShape string
}

type SkillFailureGuard struct {
	ControlTenantID      string    `json:"control_tenant_id"`
	SkillKey             string    `json:"skill_key"`
	VersionHash          string    `json:"version_hash"`
	FailureSignature     string    `json:"failure_signature"`
	FailedStepID         string    `json:"failed_step_id,omitempty"`
	ErrorCategory        string    `json:"error_category,omitempty"`
	NormalizedInputShape string    `json:"normalized_input_shape,omitempty"`
	State                string    `json:"state"`
	SourceRunID          string    `json:"source_run_id"`
	OccurrenceCount      int       `json:"occurrence_count"`
	CreatedAt            time.Time `json:"created_at"`
	LastSeenAt           time.Time `json:"last_seen_at"`
}

func (s *Store) FallbackCurrentSkill(ctx context.Context, input SkillFallbackInput) (*SkillActivation, error) {
	input.IdentityTenantID = normalizeTenant(input.IdentityTenantID)
	if input.ExecutionLane == "" {
		input.ExecutionLane = "main"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if input.WorkUnitID == "" {
		var unitID string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM run_work_units
			WHERE identity_tenant_id=? AND run_id=? AND status IN ('pending','active')
			ORDER BY CASE WHEN plan_status='in_progress' THEN 0 ELSE 1 END, sequence LIMIT 1`,
			input.IdentityTenantID, input.RunID).Scan(&unitID); err != nil {
			return nil, err
		}
		input.WorkUnitID = unitID
	}
	activation, err := activeActivationTx(ctx, tx, input.RunID, input.WorkUnitID, input.ExecutionLane)
	if err != nil {
		return nil, fmt.Errorf("active skill activation not found: %w", err)
	}
	now := time.Now().Unix()
	var goal string
	if err := tx.QueryRowContext(ctx, `SELECT goal_digest FROM run_work_units WHERE identity_tenant_id=? AND run_id=? AND id=?`,
		input.IdentityTenantID, input.RunID, input.WorkUnitID).Scan(&goal); err == nil && strings.TrimSpace(goal) != "" {
		input.NormalizedInputShape = NormalizeSkillInputShape(goal)
	} else if strings.TrimSpace(input.NormalizedInputShape) != "" {
		input.NormalizedInputShape = NormalizeSkillInputShape(input.NormalizedInputShape)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE run_skill_activations SET state='fallback', fallback_reason=?, finished_at=? WHERE id=?`,
		strings.TrimSpace(input.Reason), now, activation.ID); err != nil {
		return nil, err
	}
	normalizedCategory, repairEligible := NormalizeSkillRepairErrorCategory(input.ErrorCategory)
	if repairEligible {
		input.ErrorCategory = normalizedCategory
	}
	if signature := strings.TrimSpace(input.FailureSignature); signature != "" && repairEligible {
		if _, err := tx.ExecContext(ctx, `INSERT INTO skill_failure_guards
			(control_tenant_id, skill_key, version_hash, failure_signature, failed_step_id,
			 error_category, normalized_input_shape, state, source_run_id, created_at, occurrence_count, last_seen_at)
			VALUES(?,?,?,?,?,?,?,'active',?,?,1,?)
			ON CONFLICT(control_tenant_id, skill_key, version_hash, failure_signature) DO UPDATE SET
			 failed_step_id=excluded.failed_step_id, error_category=excluded.error_category,
			 normalized_input_shape=excluded.normalized_input_shape, state='active',
			 source_run_id=excluded.source_run_id, occurrence_count=skill_failure_guards.occurrence_count+1,
			 last_seen_at=excluded.last_seen_at`, activation.ControlTenantID, activation.SkillKey,
			activation.VersionHash, signature, strings.TrimSpace(input.FailedStepID),
			strings.TrimSpace(input.ErrorCategory), strings.TrimSpace(input.NormalizedInputShape),
			input.RunID, now, now); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	activation.State = SkillActivationFallback
	activation.FallbackReason = strings.TrimSpace(input.Reason)
	finished := time.Unix(now, 0)
	activation.FinishedAt = &finished
	return activation, nil
}

var skillRepairErrorCategories = []string{
	"stale_precondition", "invalid_procedure", "missing_failure_guard", "verification_mismatch", "schema_changed",
}

var canonicalSkillSectionOrder = []string{
	"applicability", "inputs", "preconditions", "procedure", "failure guards", "recovery", "verification",
}

// CanonicalSkillSectionOrder returns the required level-two section topology
// for curator-created Skills. Returning a copy keeps package policy immutable.
func CanonicalSkillSectionOrder() []string {
	return append([]string(nil), canonicalSkillSectionOrder...)
}

// SkillRepairErrorCategories returns the closed, deterministic defect taxonomy
// accepted by skill_fallback and automatic repair governance.
func SkillRepairErrorCategories() []string {
	return append([]string(nil), skillRepairErrorCategories...)
}

func NormalizeSkillRepairErrorCategory(category string) (string, bool) {
	category = strings.ToLower(strings.TrimSpace(category))
	for _, allowed := range skillRepairErrorCategories {
		if category == allowed {
			return category, true
		}
	}
	return "", false
}

// SkillRepairObservedFailureEligible requires a compatible pair of the
// provider-declared Skill defect and the daemon-classified tool failure. The
// model category selects a narrow repair hypothesis; it never makes an
// unrelated observed failure eligible on its own.
func SkillRepairObservedFailureEligible(repairCategory, observedCategory string) bool {
	repairCategory, ok := NormalizeSkillRepairErrorCategory(repairCategory)
	if !ok {
		return false
	}
	observedCategory = strings.ToLower(strings.TrimSpace(observedCategory))
	eligible := map[string]map[string]bool{
		"stale_precondition": {
			"command_failed": true, "not_found": true, "interface_drift": true,
		},
		"invalid_procedure": {
			"command_failed": true, "tool_schema": true, "syntax": true,
			"interface_drift": true, "check_definition": true,
		},
		"missing_failure_guard": {
			"command_failed": true, "not_found": true, "interface_drift": true,
		},
		"verification_mismatch": {
			"command_failed": true, "check_definition": true, "interface_drift": true,
		},
		"schema_changed": {
			"tool_schema": true, "interface_drift": true,
			"check_definition": true, "not_found": true,
		},
	}
	return eligible[repairCategory][observedCategory]
}

// MatchSkillFailureGuardForWorkUnit returns only an exact, active input-shape
// match. A guard never supplies a replacement command; it only suppresses a
// known-bad Skill path so the ordinary planner can take over.
func (s *Store) MatchSkillFailureGuardForWorkUnit(ctx context.Context, tenantID, skillKey, versionHash, runID, workUnitID string) (*SkillFailureGuard, error) {
	var goal string
	if err := s.db.QueryRowContext(ctx, `SELECT goal_digest FROM run_work_units WHERE identity_tenant_id=? AND run_id=? AND id=?`,
		normalizeTenant(tenantID), runID, workUnitID).Scan(&goal); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	shape := NormalizeSkillInputShape(goal)
	if shape == "" {
		return nil, nil
	}
	row := s.db.QueryRowContext(ctx, `SELECT control_tenant_id, skill_key, version_hash,
		failure_signature, failed_step_id, error_category, normalized_input_shape, state,
		source_run_id, occurrence_count, created_at, last_seen_at
		FROM skill_failure_guards WHERE control_tenant_id=? AND skill_key=? AND version_hash=?
		AND state='active' AND normalized_input_shape=? ORDER BY last_seen_at DESC, created_at DESC LIMIT 1`,
		normalizeTenant(tenantID), skillKey, versionHash, shape)
	var guard SkillFailureGuard
	var created, lastSeen int64
	if err := row.Scan(&guard.ControlTenantID, &guard.SkillKey, &guard.VersionHash,
		&guard.FailureSignature, &guard.FailedStepID, &guard.ErrorCategory,
		&guard.NormalizedInputShape, &guard.State, &guard.SourceRunID,
		&guard.OccurrenceCount, &created, &lastSeen); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	guard.CreatedAt = time.Unix(created, 0)
	guard.LastSeenAt = time.Unix(lastSeen, 0)
	return &guard, nil
}

func (s *Store) RecordSkillFailureGuardMatch(ctx context.Context, guard SkillFailureGuard) (int, error) {
	now := time.Now().Unix()
	result, err := s.db.ExecContext(ctx, `UPDATE skill_failure_guards SET occurrence_count=occurrence_count+1,
		last_seen_at=? WHERE control_tenant_id=? AND skill_key=? AND version_hash=?
		AND failure_signature=? AND state='active'`, now, guard.ControlTenantID, guard.SkillKey,
		guard.VersionHash, guard.FailureSignature)
	if err != nil {
		return 0, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return 0, fmt.Errorf("active Skill failure guard not found")
	}
	return guard.OccurrenceCount + 1, nil
}
