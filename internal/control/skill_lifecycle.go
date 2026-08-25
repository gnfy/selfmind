package control

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	SkillActivationSelected  = "selected"
	SkillActivationActive    = "active"
	SkillActivationCompleted = "completed"
	SkillActivationParked    = "parked"
	SkillActivationFallback  = "fallback"
	SkillActivationCancelled = "cancelled"

	WorkUnitPending   = "pending"
	WorkUnitActive    = "active"
	WorkUnitCompleted = "completed"
	WorkUnitParked    = "parked"
	WorkUnitFallback  = "fallback"
	WorkUnitFailed    = "failed"
	WorkUnitCancelled = "cancelled"

	TaskSkillBindingActive    = "active"
	TaskSkillBindingSuspended = "suspended"
	TaskSkillBindingReleased  = "released"
)

// SkillKey identifies the resolved logical asset, rather than trusting the
// display name alone. The identity is stable across content revisions but
// changes when resolution picks a different root/path/source.
func SkillKey(controlTenantID, name, scope, source, resolvedRoot, relativePath string) string {
	identity := strings.Join([]string{
		normalizeTenant(controlTenantID),
		strings.ToLower(strings.TrimSpace(name)),
		strings.ToLower(strings.TrimSpace(scope)),
		strings.ToLower(strings.TrimSpace(source)),
		filepath.ToSlash(filepath.Clean(strings.TrimSpace(resolvedRoot))),
		filepath.ToSlash(filepath.Clean(strings.TrimSpace(relativePath))),
	}, "\x00")
	digest := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("skill_%x", digest[:16])
}

type SkillActivation struct {
	ID                      string     `json:"id"`
	IdentityTenantID        string     `json:"identity_tenant_id"`
	ControlTenantID         string     `json:"control_tenant_id"`
	PersonID                string     `json:"person_id"`
	WorkspaceID             string     `json:"workspace_id,omitempty"`
	RunID                   string     `json:"run_id"`
	Sequence                int        `json:"sequence"`
	WorkUnitSequence        int        `json:"work_unit_sequence,omitempty"`
	WorkUnitID              string     `json:"work_unit_id"`
	ExecutionLane           string     `json:"execution_lane"`
	PrimaryTaskID           string     `json:"primary_task_id"`
	RelatedTaskID           string     `json:"related_task_id,omitempty"`
	SkillKey                string     `json:"skill_key"`
	SkillName               string     `json:"skill_name"`
	VersionHash             string     `json:"version_hash"`
	PackageHash             string     `json:"package_hash,omitempty"`
	ActivationSource        string     `json:"activation_source"`
	AttachmentMode          string     `json:"attachment_mode,omitempty"`
	DeliveryContractVersion int        `json:"delivery_contract_version,omitempty"`
	DeliveryMode            string     `json:"delivery_mode,omitempty"`
	DeliveredMain           string     `json:"-"`
	DeliveredMainHash       string     `json:"delivered_main_hash,omitempty"`
	DeliveredMainBytes      int        `json:"delivered_main_bytes,omitempty"`
	ResourceManifestJSON    string     `json:"-"`
	State                   string     `json:"state"`
	FallbackReason          string     `json:"fallback_reason,omitempty"`
	SelectedAt              time.Time  `json:"selected_at"`
	FinishedAt              *time.Time `json:"finished_at,omitempty"`
}

type ActivateSkillInput struct {
	IdentityTenantID        string
	ControlTenantID         string
	PersonID                string
	WorkspaceID             string
	RunID                   string
	WorkUnitID              string
	ExecutionLane           string
	SkillKey                string
	SkillName               string
	VersionHash             string
	PackageHash             string
	ActivationSource        string
	AttachmentMode          string
	DeliveryContractVersion int
	DeliveryMode            string
	DeliveredMain           string
	DeliveredMainHash       string
	DeliveredMainBytes      int
	ResourceManifestJSON    string
	ContentRef              string
	ContentBody             string
	CreatedBy               string
}

type TaskSkillBinding struct {
	IdentityTenantID        string    `json:"identity_tenant_id"`
	PersonID                string    `json:"person_id"`
	TaskID                  string    `json:"task_id"`
	ControlTenantID         string    `json:"control_tenant_id"`
	WorkspaceID             string    `json:"workspace_id,omitempty"`
	SkillKey                string    `json:"skill_key"`
	SkillName               string    `json:"skill_name"`
	State                   string    `json:"state"`
	BindingSource           string    `json:"binding_source"`
	BoundFromRunID          string    `json:"bound_from_run_id,omitempty"`
	LastResolvedVersionHash string    `json:"last_resolved_version_hash,omitempty"`
	SuspendedReason         string    `json:"suspended_reason,omitempty"`
	CreatedAt               time.Time `json:"created_at"`
	UpdatedAt               time.Time `json:"updated_at"`
}

type BindTaskSkillInput struct {
	IdentityTenantID string
	PersonID         string
	TaskID           string
	ControlTenantID  string
	WorkspaceID      string
	SkillKey         string
	SkillName        string
	BindingSource    string
	BoundFromRunID   string
	VersionHash      string
}

type skillLifecycleScanner interface{ Scan(...interface{}) error }

func (s *Store) ActivateSkill(ctx context.Context, input ActivateSkillInput) (*SkillActivation, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("control store is required")
	}
	input.IdentityTenantID = normalizeTenant(input.IdentityTenantID)
	input.ControlTenantID = normalizeTenant(input.ControlTenantID)
	input.ExecutionLane = strings.TrimSpace(input.ExecutionLane)
	if input.ExecutionLane == "" {
		input.ExecutionLane = "main"
	}
	if input.RunID == "" || input.PersonID == "" || input.SkillKey == "" || input.SkillName == "" || input.VersionHash == "" {
		return nil, fmt.Errorf("run, person, skill key, name, and version are required")
	}
	if input.DeliveryContractVersion > 0 {
		digest := sha256.Sum256([]byte(input.DeliveredMain))
		if input.DeliveredMain == "" || input.DeliveredMainHash != fmt.Sprintf("%x", digest[:]) || input.DeliveredMainBytes != len(input.DeliveredMain) {
			return nil, fmt.Errorf("Skill delivery receipt does not match delivered main bytes")
		}
		if !json.Valid([]byte(input.ResourceManifestJSON)) {
			return nil, fmt.Errorf("Skill delivery resource manifest must be valid JSON")
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var unit RunWorkUnit
	query := `SELECT id, identity_tenant_id, person_id, workspace_id, run_id, sequence,
			primary_task_id, related_task_id, goal_digest, plan_status, status, outcome_summary,
			verification_state, verification_refs_json, started_at, created_at, finished_at,
			started_cursor, finished_cursor
		FROM run_work_units WHERE identity_tenant_id=? AND run_id=?`
	args := []interface{}{input.IdentityTenantID, input.RunID}
	if strings.TrimSpace(input.WorkUnitID) != "" {
		query += ` AND id=?`
		args = append(args, strings.TrimSpace(input.WorkUnitID))
	} else {
		query += ` AND status IN ('pending','active') ORDER BY CASE WHEN plan_status='in_progress' THEN 0 ELSE 1 END, sequence LIMIT 1`
	}
	unitPtr, err := scanRunWorkUnit(tx.QueryRowContext(ctx, query, args...))
	if err != nil {
		return nil, fmt.Errorf("resolve work unit: %w", err)
	}
	unit = *unitPtr
	if unit.PersonID != input.PersonID {
		return nil, fmt.Errorf("work unit is not owned by person")
	}
	if workUnitTerminal(unit.Status) {
		return nil, fmt.Errorf("work unit %s is already terminal (%s)", unit.ID, unit.Status)
	}
	input.WorkUnitID = unit.ID
	if input.WorkspaceID == "" {
		input.WorkspaceID = unit.WorkspaceID
	}
	var priorFallbacks int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM run_skill_activations
		WHERE run_id=? AND work_unit_id=? AND execution_lane=? AND state='fallback'`,
		input.RunID, unit.ID, input.ExecutionLane).Scan(&priorFallbacks); err != nil {
		return nil, err
	}
	if priorFallbacks > 0 {
		return nil, fmt.Errorf("work unit %s already fell back from a skill; complete it with ordinary planning before selecting another", unit.ID)
	}

	if existing, err := activeActivationTx(ctx, tx, input.RunID, unit.ID, input.ExecutionLane); err == nil {
		if existing.SkillKey == input.SkillKey && existing.VersionHash == input.VersionHash {
			return existing, nil
		}
		return nil, fmt.Errorf("work unit %s already has active skill %s; complete or fallback it before selecting another", unit.ID, existing.SkillName)
	} else if err != sql.ErrNoRows {
		return nil, err
	}

	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `UPDATE skill_versions SET state='previous'
		WHERE control_tenant_id=? AND skill_key=? AND state='active' AND version_hash<>?`,
		input.ControlTenantID, input.SkillKey, input.VersionHash); err != nil {
		return nil, err
	}
	createdBy := strings.TrimSpace(input.CreatedBy)
	if createdBy == "" {
		createdBy = "external_reconcile"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO skill_versions
		(control_tenant_id, skill_key, skill_name, version_hash, state, content_ref, content_body,
		 package_hash, resource_manifest_json, created_by, created_at, promoted_at)
		VALUES(?,?,?,?, 'active', ?,?,?,?,?,?,?)
		ON CONFLICT(control_tenant_id, skill_key, version_hash) DO UPDATE SET
		 skill_name=excluded.skill_name, state='active', content_ref=excluded.content_ref,
		 content_body=excluded.content_body, package_hash=excluded.package_hash,
		 resource_manifest_json=excluded.resource_manifest_json,
		 promoted_at=COALESCE(skill_versions.promoted_at, excluded.promoted_at)`,
		input.ControlTenantID, input.SkillKey, input.SkillName, input.VersionHash,
		input.ContentRef, input.ContentBody, input.PackageHash, input.ResourceManifestJSON, createdBy, now, now); err != nil {
		return nil, err
	}
	var sequence int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM run_skill_activations WHERE run_id=?`, input.RunID).Scan(&sequence); err != nil {
		return nil, err
	}
	activation := &SkillActivation{
		ID: "activation_" + uuid.NewString(), IdentityTenantID: input.IdentityTenantID,
		ControlTenantID: input.ControlTenantID, PersonID: input.PersonID, WorkspaceID: input.WorkspaceID,
		RunID: input.RunID, Sequence: sequence, WorkUnitID: unit.ID, ExecutionLane: input.ExecutionLane,
		WorkUnitSequence: unit.Sequence,
		PrimaryTaskID:    unit.PrimaryTaskID, RelatedTaskID: unit.RelatedTaskID, SkillKey: input.SkillKey,
		SkillName: input.SkillName, VersionHash: input.VersionHash, PackageHash: input.PackageHash,
		ActivationSource: strings.TrimSpace(input.ActivationSource), AttachmentMode: strings.TrimSpace(input.AttachmentMode),
		DeliveryContractVersion: input.DeliveryContractVersion, DeliveryMode: input.DeliveryMode,
		DeliveredMain: input.DeliveredMain, DeliveredMainHash: input.DeliveredMainHash,
		DeliveredMainBytes: input.DeliveredMainBytes, ResourceManifestJSON: input.ResourceManifestJSON,
		State: SkillActivationActive, SelectedAt: time.Unix(now, 0),
	}
	if activation.ActivationSource == "" {
		activation.ActivationSource = "model"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO run_skill_activations
		(id, identity_tenant_id, control_tenant_id, person_id, workspace_id, run_id, sequence,
		 work_unit_id, execution_lane, primary_task_id, related_task_id, skill_key, skill_name,
		 version_hash, package_hash, activation_source, attachment_mode, delivery_contract_version,
		 delivery_mode, delivered_main, delivered_main_hash, delivered_main_bytes, resource_manifest_json,
		 state, selected_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, activation.ID, activation.IdentityTenantID,
		activation.ControlTenantID, activation.PersonID, activation.WorkspaceID, activation.RunID,
		activation.Sequence, activation.WorkUnitID, activation.ExecutionLane, activation.PrimaryTaskID,
		activation.RelatedTaskID, activation.SkillKey, activation.SkillName, activation.VersionHash,
		activation.PackageHash, activation.ActivationSource, activation.AttachmentMode,
		activation.DeliveryContractVersion, activation.DeliveryMode, activation.DeliveredMain,
		activation.DeliveredMainHash, activation.DeliveredMainBytes, activation.ResourceManifestJSON,
		activation.State, now); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE run_work_units SET status='active' WHERE id=? AND status='pending'`, unit.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return activation, nil
}

func activeActivationTx(ctx context.Context, tx *sql.Tx, runID, workUnitID, lane string) (*SkillActivation, error) {
	row := tx.QueryRowContext(ctx, `SELECT id, identity_tenant_id, control_tenant_id, person_id,
		workspace_id, run_id, sequence, work_unit_id, execution_lane, primary_task_id,
		related_task_id, skill_key, skill_name, version_hash, activation_source, attachment_mode,
		state, fallback_reason, selected_at, finished_at, package_hash, delivery_contract_version,
		delivery_mode, delivered_main, delivered_main_hash, delivered_main_bytes, resource_manifest_json FROM run_skill_activations
		WHERE run_id=? AND work_unit_id=? AND execution_lane=? AND state IN ('selected','active') LIMIT 1`, runID, workUnitID, lane)
	return scanSkillActivation(row)
}

func scanSkillActivation(row skillLifecycleScanner) (*SkillActivation, error) {
	var a SkillActivation
	var selected int64
	var finished sql.NullInt64
	if err := row.Scan(&a.ID, &a.IdentityTenantID, &a.ControlTenantID, &a.PersonID,
		&a.WorkspaceID, &a.RunID, &a.Sequence, &a.WorkUnitID, &a.ExecutionLane,
		&a.PrimaryTaskID, &a.RelatedTaskID, &a.SkillKey, &a.SkillName, &a.VersionHash,
		&a.ActivationSource, &a.AttachmentMode, &a.State, &a.FallbackReason, &selected, &finished,
		&a.PackageHash, &a.DeliveryContractVersion, &a.DeliveryMode, &a.DeliveredMain,
		&a.DeliveredMainHash, &a.DeliveredMainBytes, &a.ResourceManifestJSON); err != nil {
		return nil, err
	}
	a.SelectedAt = time.Unix(selected, 0)
	if finished.Valid {
		at := time.Unix(finished.Int64, 0)
		a.FinishedAt = &at
	}
	return &a, nil
}

func (s *Store) ActiveSkillActivation(ctx context.Context, tenantID, runID, workUnitID, lane string) (*SkillActivation, error) {
	if lane == "" {
		lane = "main"
	}
	row := s.db.QueryRowContext(ctx, `SELECT id, identity_tenant_id, control_tenant_id, person_id,
		workspace_id, run_id, sequence, work_unit_id, execution_lane, primary_task_id,
		related_task_id, skill_key, skill_name, version_hash, activation_source, attachment_mode,
		state, fallback_reason, selected_at, finished_at, package_hash, delivery_contract_version,
		delivery_mode, delivered_main, delivered_main_hash, delivered_main_bytes, resource_manifest_json FROM run_skill_activations
		WHERE identity_tenant_id=? AND run_id=? AND work_unit_id=? AND execution_lane=?
		AND state IN ('selected','active') LIMIT 1`, normalizeTenant(tenantID), runID, workUnitID, lane)
	activation, err := scanSkillActivation(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return activation, err
}

func (s *Store) BindTaskSkill(ctx context.Context, input BindTaskSkillInput) (*TaskSkillBinding, error) {
	input.IdentityTenantID = normalizeTenant(input.IdentityTenantID)
	input.ControlTenantID = normalizeTenant(input.ControlTenantID)
	if input.PersonID == "" || input.TaskID == "" || input.SkillKey == "" || input.SkillName == "" {
		return nil, fmt.Errorf("person, task, skill key, and skill name are required")
	}
	if input.BindingSource == "" {
		input.BindingSource = "explicit"
	}
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `INSERT INTO task_skill_bindings
		(identity_tenant_id, person_id, task_id, control_tenant_id, workspace_id, skill_key,
		 skill_name, state, binding_source, bound_from_run_id, last_resolved_version_hash,
		 suspended_reason, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,'active',?,?,?,'',?,?)
		ON CONFLICT(identity_tenant_id, person_id, task_id) DO UPDATE SET
		 control_tenant_id=excluded.control_tenant_id, workspace_id=excluded.workspace_id,
		 skill_key=excluded.skill_key, skill_name=excluded.skill_name, state='active',
		 binding_source=excluded.binding_source, bound_from_run_id=excluded.bound_from_run_id,
		 last_resolved_version_hash=excluded.last_resolved_version_hash, suspended_reason='', updated_at=excluded.updated_at`,
		input.IdentityTenantID, input.PersonID, input.TaskID, input.ControlTenantID, input.WorkspaceID,
		input.SkillKey, input.SkillName, input.BindingSource, input.BoundFromRunID, input.VersionHash, now, now)
	if err != nil {
		return nil, err
	}
	return s.GetTaskSkillBinding(ctx, input.IdentityTenantID, input.PersonID, input.TaskID)
}

func (s *Store) GetTaskSkillBinding(ctx context.Context, tenantID, personID, taskID string) (*TaskSkillBinding, error) {
	row := s.db.QueryRowContext(ctx, `SELECT identity_tenant_id, person_id, task_id, control_tenant_id,
		workspace_id, skill_key, skill_name, state, binding_source, bound_from_run_id,
		last_resolved_version_hash, suspended_reason, created_at, updated_at
		FROM task_skill_bindings WHERE identity_tenant_id=? AND person_id=? AND task_id=?`,
		normalizeTenant(tenantID), personID, taskID)
	var binding TaskSkillBinding
	var created, updated int64
	if err := row.Scan(&binding.IdentityTenantID, &binding.PersonID, &binding.TaskID,
		&binding.ControlTenantID, &binding.WorkspaceID, &binding.SkillKey, &binding.SkillName,
		&binding.State, &binding.BindingSource, &binding.BoundFromRunID,
		&binding.LastResolvedVersionHash, &binding.SuspendedReason, &created, &updated); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	binding.CreatedAt = time.Unix(created, 0)
	binding.UpdatedAt = time.Unix(updated, 0)
	return &binding, nil
}

func (s *Store) TaskSkillBindingForWorkUnit(ctx context.Context, tenantID, personID, runID, workUnitID string) (*TaskSkillBinding, error) {
	var relatedTaskID string
	query := `SELECT related_task_id FROM run_work_units WHERE identity_tenant_id=? AND person_id=? AND run_id=?`
	args := []interface{}{normalizeTenant(tenantID), personID, runID}
	if strings.TrimSpace(workUnitID) != "" {
		query += ` AND id=?`
		args = append(args, strings.TrimSpace(workUnitID))
	} else {
		query += ` AND status IN ('pending','active') ORDER BY CASE WHEN plan_status='in_progress' THEN 0 ELSE 1 END, sequence LIMIT 1`
	}
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&relatedTaskID); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if strings.TrimSpace(relatedTaskID) == "" {
		return nil, nil
	}
	return s.GetTaskSkillBinding(ctx, tenantID, personID, relatedTaskID)
}

func (s *Store) SetTaskSkillBindingState(ctx context.Context, tenantID, personID, taskID, state, reason string) error {
	if state != TaskSkillBindingActive && state != TaskSkillBindingSuspended && state != TaskSkillBindingReleased {
		return fmt.Errorf("invalid task skill binding state %q", state)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE task_skill_bindings SET state=?, suspended_reason=?, updated_at=?
		WHERE identity_tenant_id=? AND person_id=? AND task_id=?`, state, strings.TrimSpace(reason),
		time.Now().Unix(), normalizeTenant(tenantID), personID, taskID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return fmt.Errorf("task skill binding not found")
	}
	return nil
}

func (s *Store) RecordTaskSkillBindingResolved(ctx context.Context, tenantID, personID, taskID, versionHash string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE task_skill_bindings SET last_resolved_version_hash=?,
		suspended_reason='', state='active', updated_at=? WHERE identity_tenant_id=? AND person_id=? AND task_id=?`,
		strings.TrimSpace(versionHash), time.Now().Unix(), normalizeTenant(tenantID), personID, taskID)
	return err
}

func finalizeRunSkillLifecycleTx(ctx context.Context, tx *sql.Tx, input RunFinalization, personID string, now time.Time, allowBinding bool) error {
	verification := strings.TrimSpace(input.VerificationState)
	if verification == "" {
		verification = "not_applicable"
	}
	outcomeClass := classifyEvolutionOutcome(input.RunStatus)
	cleanOutcome := !input.ClaimMismatch
	switch verification {
	case "failed", "stale", "blocked", "partial":
		cleanOutcome = false
	}
	success := outcomeClass == evolutionOutcomeSuccess && cleanOutcome
	unitState := WorkUnitFailed
	activationState := SkillActivationFallback
	fallbackReason := "run ended with status " + strings.TrimSpace(input.RunStatus)
	switch {
	case success:
		unitState = WorkUnitCompleted
		activationState = SkillActivationCompleted
		fallbackReason = ""
	case outcomeClass == evolutionOutcomeParked && cleanOutcome:
		unitState = WorkUnitParked
		activationState = SkillActivationParked
		fallbackReason = ""
	case outcomeClass == evolutionOutcomeCancelled && !input.ClaimMismatch:
		unitState = WorkUnitCancelled
		activationState = SkillActivationCancelled
		fallbackReason = "run was cancelled"
	}
	refs, _ := json.Marshal(input.VerificationRefs)
	if _, err := tx.ExecContext(ctx, `UPDATE run_work_units SET status=?, outcome_summary=?,
		verification_state=?, verification_refs_json=?, finished_at=?
		WHERE identity_tenant_id=? AND run_id=? AND status IN ('pending','active')`,
		unitState, strings.TrimSpace(input.Summary), verification, string(refs), now.Unix(),
		normalizeTenant(input.Identity.TenantID), input.RunID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE run_skill_activations SET state=?, fallback_reason=?, finished_at=?
		WHERE identity_tenant_id=? AND run_id=? AND state IN ('selected','active')`,
		activationState, fallbackReason, now.Unix(), normalizeTenant(input.Identity.TenantID), input.RunID); err != nil {
		return err
	}
	// Candidate refs are guaranteed for the work-unit lifetime, not forever.
	// The terminal transaction is the authoritative bounded cleanup point and
	// keeps long-running installations from retaining every discovery snapshot.
	if _, err := tx.ExecContext(ctx, `DELETE FROM skill_candidate_refs
		WHERE identity_tenant_id=? AND run_id=?`, normalizeTenant(input.Identity.TenantID), input.RunID); err != nil {
		return err
	}
	if !success || !allowBinding {
		return nil
	}
	// A successful run may establish affinity only for an existing active Skill
	// and only when the related task has no binding. Conflicts are preserved for
	// audit instead of silently replacing another task default.
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT a.control_tenant_id, a.related_task_id,
		a.workspace_id, a.skill_key, a.skill_name, a.version_hash
		FROM run_skill_activations a
		JOIN skill_versions v ON v.control_tenant_id=a.control_tenant_id
		 AND v.skill_key=a.skill_key AND v.version_hash=a.version_hash AND v.state='active'
		WHERE a.identity_tenant_id=? AND a.run_id=? AND a.state='completed' AND a.related_task_id<>''`,
		normalizeTenant(input.Identity.TenantID), input.RunID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var controlTenantID, taskID, workspaceID, skillKey, skillName, versionHash string
		if err := rows.Scan(&controlTenantID, &taskID, &workspaceID, &skillKey, &skillName, &versionHash); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO task_skill_bindings
			(identity_tenant_id, person_id, task_id, control_tenant_id, workspace_id,
			 skill_key, skill_name, state, binding_source, bound_from_run_id,
			 last_resolved_version_hash, suspended_reason, created_at, updated_at)
			VALUES(?,?,?,?,?,?,?,'active','successful_run',?,?,'',?,?)`,
			normalizeTenant(input.Identity.TenantID), personID, taskID, controlTenantID,
			workspaceID, skillKey, skillName, input.RunID, versionHash, now.Unix(), now.Unix()); err != nil {
			return err
		}
	}
	return rows.Err()
}
