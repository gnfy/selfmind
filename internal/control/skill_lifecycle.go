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

type RunWorkUnit struct {
	ID                string          `json:"id"`
	IdentityTenantID  string          `json:"identity_tenant_id"`
	PersonID          string          `json:"person_id"`
	WorkspaceID       string          `json:"workspace_id,omitempty"`
	RunID             string          `json:"run_id"`
	Sequence          int             `json:"sequence"`
	PrimaryTaskID     string          `json:"primary_task_id"`
	RelatedTaskID     string          `json:"related_task_id,omitempty"`
	GoalDigest        string          `json:"goal_digest,omitempty"`
	PlanStatus        string          `json:"plan_status,omitempty"`
	Status            string          `json:"status"`
	OutcomeSummary    string          `json:"outcome_summary,omitempty"`
	VerificationState string          `json:"verification_state,omitempty"`
	VerificationRefs  json.RawMessage `json:"verification_refs,omitempty"`
	StartedAt         *time.Time      `json:"started_at,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
	FinishedAt        *time.Time      `json:"finished_at,omitempty"`
	StartedCursor     int64           `json:"started_cursor,omitempty"`
	FinishedCursor    int64           `json:"finished_cursor,omitempty"`
}

type WorkUnitPlanInput struct {
	WorkUnitID    string
	GoalDigest    string
	PlanStatus    string
	RelatedTaskID string
}

type SkillActivation struct {
	ID               string     `json:"id"`
	IdentityTenantID string     `json:"identity_tenant_id"`
	ControlTenantID  string     `json:"control_tenant_id"`
	PersonID         string     `json:"person_id"`
	WorkspaceID      string     `json:"workspace_id,omitempty"`
	RunID            string     `json:"run_id"`
	Sequence         int        `json:"sequence"`
	WorkUnitSequence int        `json:"work_unit_sequence,omitempty"`
	WorkUnitID       string     `json:"work_unit_id"`
	ExecutionLane    string     `json:"execution_lane"`
	PrimaryTaskID    string     `json:"primary_task_id"`
	RelatedTaskID    string     `json:"related_task_id,omitempty"`
	SkillKey         string     `json:"skill_key"`
	SkillName        string     `json:"skill_name"`
	VersionHash      string     `json:"version_hash"`
	ActivationSource string     `json:"activation_source"`
	AttachmentMode   string     `json:"attachment_mode,omitempty"`
	State            string     `json:"state"`
	FallbackReason   string     `json:"fallback_reason,omitempty"`
	SelectedAt       time.Time  `json:"selected_at"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
}

type ActivateSkillInput struct {
	IdentityTenantID string
	ControlTenantID  string
	PersonID         string
	WorkspaceID      string
	RunID            string
	WorkUnitID       string
	ExecutionLane    string
	SkillKey         string
	SkillName        string
	VersionHash      string
	ActivationSource string
	AttachmentMode   string
	ContentRef       string
	ContentBody      string
	CreatedBy        string
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

// SyncRunWorkUnits projects the model's complete work-unit snapshot. Plan
// status closes the execution window; verification is independently derived
// from durable evidence inside that window rather than trusted from plan prose.
// Stable ids returned by update_plan win, followed by deterministic task/goal
// identity. Array position is never allowed to silently retarget an old unit.
func (s *Store) SyncRunWorkUnits(ctx context.Context, tenantID, runID string, plan []WorkUnitPlanInput) ([]RunWorkUnit, error) {
	if s == nil || s.db == nil || strings.TrimSpace(runID) == "" || len(plan) == 0 {
		return nil, nil
	}
	tenant := normalizeTenant(tenantID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var personID, workspaceID, primaryTaskID string
	if err := tx.QueryRowContext(ctx, `SELECT person_id, COALESCE(workspace_id,''), task_id FROM task_runs WHERE tenant_id=? AND id=?`, tenant, runID).
		Scan(&personID, &workspaceID, &primaryTaskID); err != nil {
		return nil, err
	}
	existing, err := listRunWorkUnitsTx(ctx, tx, tenant, runID)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*RunWorkUnit, len(existing))
	for i := range existing {
		byID[existing[i].ID] = &existing[i]
	}
	// Free the positive sequence namespace before applying the new complete
	// snapshot. This lets explicit stable ids survive insertions and reordering.
	if _, err := tx.ExecContext(ctx, `UPDATE run_work_units SET sequence=-sequence WHERE run_id=? AND sequence>0`, runID); err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	currentCursor, err := maxRunEventCursorTx(ctx, tx, runID)
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	for i, item := range plan {
		sequence := i + 1
		item.WorkUnitID = strings.TrimSpace(item.WorkUnitID)
		item.GoalDigest = strings.TrimSpace(item.GoalDigest)
		item.PlanStatus = strings.TrimSpace(item.PlanStatus)
		item.RelatedTaskID = strings.TrimSpace(item.RelatedTaskID)
		if sequence == 1 && item.RelatedTaskID == "" {
			item.RelatedTaskID = primaryTaskID
		}
		if item.RelatedTaskID != "" {
			var owned int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE tenant_id=? AND person_id=? AND id=?`, tenant, personID, item.RelatedTaskID).Scan(&owned); err != nil {
				return nil, err
			}
			if owned != 1 {
				return nil, fmt.Errorf("related task %s is not owned by run person", item.RelatedTaskID)
			}
		}
		unit, err := resolvePlanWorkUnit(item, existing, byID, used)
		if err != nil {
			return nil, err
		}
		if unit == nil {
			unit = &RunWorkUnit{
				ID: "wu_" + uuid.NewString(), IdentityTenantID: tenant, PersonID: personID,
				WorkspaceID: workspaceID, RunID: runID, PrimaryTaskID: primaryTaskID,
				CreatedAt: time.Unix(now, 0), Status: WorkUnitPending,
			}
			byID[unit.ID] = unit
		}
		if used[unit.ID] {
			return nil, fmt.Errorf("work unit %s appears more than once in the plan snapshot", unit.ID)
		}
		used[unit.ID] = true
		unit.Sequence = sequence
		unit.RelatedTaskID = item.RelatedTaskID
		unit.GoalDigest = item.GoalDigest
		unit.PlanStatus = item.PlanStatus
		if err := upsertProjectedWorkUnitTx(ctx, tx, unit, currentCursor, now); err != nil {
			return nil, err
		}
	}
	remainingSequence := len(plan)
	for i := range existing {
		unit := &existing[i]
		if used[unit.ID] {
			continue
		}
		remainingSequence++
		unit.Sequence = remainingSequence
		unit.PlanStatus = "cancelled"
		if err := upsertProjectedWorkUnitTx(ctx, tx, unit, currentCursor, now); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.ListRunWorkUnits(ctx, tenant, runID)
}

func resolvePlanWorkUnit(item WorkUnitPlanInput, existing []RunWorkUnit, byID map[string]*RunWorkUnit, used map[string]bool) (*RunWorkUnit, error) {
	if item.WorkUnitID != "" {
		unit := byID[item.WorkUnitID]
		if unit == nil {
			return nil, fmt.Errorf("work unit %s does not belong to this run", item.WorkUnitID)
		}
		return unit, nil
	}
	if item.RelatedTaskID != "" {
		for i := range existing {
			if !used[existing[i].ID] && existing[i].RelatedTaskID == item.RelatedTaskID {
				return &existing[i], nil
			}
		}
	}
	goal := normalizeWorkUnitGoal(item.GoalDigest)
	if goal != "" {
		for i := range existing {
			if !used[existing[i].ID] && normalizeWorkUnitGoal(existing[i].GoalDigest) == goal {
				return &existing[i], nil
			}
		}
	}
	return nil, nil
}

func normalizeWorkUnitGoal(goal string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(goal))), " ")
}

func upsertProjectedWorkUnitTx(ctx context.Context, tx *sql.Tx, unit *RunWorkUnit, finishCursor, now int64) error {
	status := unit.Status
	startedAt := nullableUnix(unit.StartedAt)
	finishedAt := nullableUnix(unit.FinishedAt)
	startedCursor := unit.StartedCursor
	finishedCursor := unit.FinishedCursor
	verification := unit.VerificationState
	refs := string(unit.VerificationRefs)
	if refs == "" {
		refs = "[]"
	}
	switch unit.PlanStatus {
	case "in_progress":
		if !workUnitTerminal(status) {
			status = WorkUnitActive
			if startedAt == nil {
				startedAt = now
				startedCursor = finishCursor
			}
		}
	case "pending":
		if !workUnitTerminal(status) {
			status = WorkUnitPending
		}
	case "completed", "cancelled":
		if !workUnitTerminal(status) {
			if unit.PlanStatus == "completed" {
				status = WorkUnitCompleted
				verification, refs = workUnitEvidenceProjectionTx(ctx, tx, unit.RunID, startedCursor, finishCursor)
			} else {
				status = WorkUnitCancelled
				verification, refs = "", "[]"
			}
			finishedAt = now
			finishedCursor = finishCursor
			activationState := SkillActivationCompleted
			reason := ""
			if status == WorkUnitCancelled {
				activationState = SkillActivationCancelled
				reason = "work unit cancelled by plan"
			}
			if _, err := tx.ExecContext(ctx, `UPDATE run_skill_activations SET state=?, fallback_reason=?, finished_at=?
				WHERE run_id=? AND work_unit_id=? AND state IN ('selected','active')`, activationState, reason, now, unit.RunID, unit.ID); err != nil {
				return err
			}
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO run_work_units
		(id, identity_tenant_id, person_id, workspace_id, run_id, sequence, primary_task_id,
		 related_task_id, goal_digest, plan_status, status, outcome_summary, verification_state,
		 verification_refs_json, started_at, created_at, finished_at, started_cursor, finished_cursor)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET sequence=excluded.sequence, related_task_id=excluded.related_task_id,
		 goal_digest=excluded.goal_digest, plan_status=excluded.plan_status, status=excluded.status,
		 outcome_summary=CASE WHEN excluded.status='completed' AND run_work_units.outcome_summary='' THEN excluded.goal_digest ELSE run_work_units.outcome_summary END,
		 verification_state=excluded.verification_state, verification_refs_json=excluded.verification_refs_json,
		 started_at=COALESCE(run_work_units.started_at, excluded.started_at), finished_at=excluded.finished_at,
		 started_cursor=CASE WHEN run_work_units.started_cursor=0 THEN excluded.started_cursor ELSE run_work_units.started_cursor END,
		 finished_cursor=excluded.finished_cursor`,
		unit.ID, unit.IdentityTenantID, unit.PersonID, unit.WorkspaceID, unit.RunID, unit.Sequence,
		unit.PrimaryTaskID, unit.RelatedTaskID, unit.GoalDigest, unit.PlanStatus, status,
		unit.OutcomeSummary, verification, refs, startedAt, unit.CreatedAt.Unix(), finishedAt,
		startedCursor, finishedCursor)
	return err
}

func nullableUnix(value *time.Time) interface{} {
	if value == nil {
		return nil
	}
	return value.Unix()
}

func workUnitTerminal(status string) bool {
	switch status {
	case WorkUnitCompleted, WorkUnitParked, WorkUnitFallback, WorkUnitFailed, WorkUnitCancelled:
		return true
	default:
		return false
	}
}

func maxRunEventCursorTx(ctx context.Context, tx *sql.Tx, runID string) (int64, error) {
	var cursor int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(cursor),0) FROM task_events WHERE run_id=?`, runID).Scan(&cursor)
	return cursor, err
}

type workUnitEvidence struct {
	Kind       string `json:"kind"`
	Status     string `json:"status"`
	StartedAt  int64  `json:"started_at_unix_nano"`
	FinishedAt int64  `json:"finished_at_unix_nano"`
	Files      []struct {
		BeforeSHA256 string `json:"before_sha256"`
		AfterSHA256  string `json:"after_sha256"`
	} `json:"files"`
	Command *struct {
		Command string `json:"command"`
		CWD     string `json:"cwd"`
		Kind    string `json:"kind"`
	} `json:"command"`
}

func workUnitEvidenceProjectionTx(ctx context.Context, tx *sql.Tx, runID string, startedCursor, finishedCursor int64) (string, string) {
	rows, err := tx.QueryContext(ctx, `SELECT COALESCE(payload_json,'{}') FROM task_events
		WHERE run_id=? AND COALESCE(cursor,0)>? AND COALESCE(cursor,0)<=? AND type='evidence.recorded'
		ORDER BY COALESCE(cursor,0), rowid`, runID, startedCursor, finishedCursor)
	if err != nil {
		return "", "[]"
	}
	defer rows.Close()
	latestMutation := int64(0)
	checks := map[string]workUnitEvidence{}
	order := []string{}
	for rows.Next() {
		var raw string
		if rows.Scan(&raw) != nil {
			continue
		}
		var payload struct {
			Evidence workUnitEvidence `json:"evidence"`
		}
		if json.Unmarshal([]byte(raw), &payload) != nil {
			continue
		}
		evidence := payload.Evidence
		if evidence.Kind == "mutation" && evidence.Status == "succeeded" {
			for _, file := range evidence.Files {
				if file.BeforeSHA256 != file.AfterSHA256 && evidence.FinishedAt > latestMutation {
					latestMutation = evidence.FinishedAt
				}
			}
		}
		if evidence.Kind != "verification" || evidence.Command == nil {
			continue
		}
		key := strings.Join([]string{evidence.Command.Kind, evidence.Command.Command, evidence.Command.CWD}, "\x00")
		if _, ok := checks[key]; !ok {
			order = append(order, key)
		}
		if previous, ok := checks[key]; !ok || evidence.StartedAt >= previous.StartedAt {
			checks[key] = evidence
		}
	}
	passed, failed, blocked := 0, 0, 0
	refs := make([]string, 0, len(order))
	for _, key := range order {
		check := checks[key]
		if check.StartedAt < latestMutation {
			continue
		}
		if command := strings.TrimSpace(check.Command.Command); command != "" {
			refs = append(refs, command)
		}
		switch check.Status {
		case "succeeded":
			passed++
		case "blocked":
			blocked++
		default:
			failed++
		}
	}
	refsJSON, _ := json.Marshal(refs)
	switch {
	case failed > 0:
		return "failed", string(refsJSON)
	case passed > 0 && blocked == 0:
		return "passed", string(refsJSON)
	case passed == 0 && blocked > 0:
		return "blocked", string(refsJSON)
	case passed > 0 && blocked > 0:
		return "partial", string(refsJSON)
	case latestMutation > 0:
		return "not_run", string(refsJSON)
	default:
		return "not_applicable", string(refsJSON)
	}
}

func (s *Store) ListRunWorkUnits(ctx context.Context, tenantID, runID string) ([]RunWorkUnit, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, identity_tenant_id, person_id, workspace_id,
		run_id, sequence, primary_task_id, related_task_id, goal_digest, plan_status, status,
		outcome_summary, verification_state, verification_refs_json, started_at, created_at, finished_at,
		started_cursor, finished_cursor
		FROM run_work_units WHERE identity_tenant_id=? AND run_id=? ORDER BY sequence`, normalizeTenant(tenantID), runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunWorkUnit
	for rows.Next() {
		unit, err := scanRunWorkUnit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *unit)
	}
	return out, rows.Err()
}

func listRunWorkUnitsTx(ctx context.Context, tx *sql.Tx, tenantID, runID string) ([]RunWorkUnit, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, identity_tenant_id, person_id, workspace_id,
		run_id, sequence, primary_task_id, related_task_id, goal_digest, plan_status, status,
		outcome_summary, verification_state, verification_refs_json, started_at, created_at, finished_at,
		started_cursor, finished_cursor
		FROM run_work_units WHERE identity_tenant_id=? AND run_id=? ORDER BY sequence`, tenantID, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunWorkUnit
	for rows.Next() {
		unit, err := scanRunWorkUnit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *unit)
	}
	return out, rows.Err()
}

func (s *Store) CurrentRunWorkUnit(ctx context.Context, tenantID, runID string) (*RunWorkUnit, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, identity_tenant_id, person_id, workspace_id,
		run_id, sequence, primary_task_id, related_task_id, goal_digest, plan_status, status,
		outcome_summary, verification_state, verification_refs_json, started_at, created_at, finished_at,
		started_cursor, finished_cursor
		FROM run_work_units WHERE identity_tenant_id=? AND run_id=? AND status IN ('pending','active')
		ORDER BY CASE WHEN plan_status='in_progress' THEN 0 WHEN status='active' THEN 1 ELSE 2 END, sequence LIMIT 1`,
		normalizeTenant(tenantID), runID)
	unit, err := scanRunWorkUnit(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return unit, err
}

type skillLifecycleScanner interface{ Scan(...interface{}) error }

func scanRunWorkUnit(row skillLifecycleScanner) (*RunWorkUnit, error) {
	var unit RunWorkUnit
	var refs string
	var created int64
	var started, finished sql.NullInt64
	if err := row.Scan(&unit.ID, &unit.IdentityTenantID, &unit.PersonID, &unit.WorkspaceID,
		&unit.RunID, &unit.Sequence, &unit.PrimaryTaskID, &unit.RelatedTaskID, &unit.GoalDigest,
		&unit.PlanStatus, &unit.Status, &unit.OutcomeSummary, &unit.VerificationState, &refs,
		&started, &created, &finished, &unit.StartedCursor, &unit.FinishedCursor); err != nil {
		return nil, err
	}
	unit.VerificationRefs = json.RawMessage(refs)
	unit.CreatedAt = time.Unix(created, 0)
	if started.Valid {
		at := time.Unix(started.Int64, 0)
		unit.StartedAt = &at
	}
	if finished.Valid {
		at := time.Unix(finished.Int64, 0)
		unit.FinishedAt = &at
	}
	return &unit, nil
}

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
		(control_tenant_id, skill_key, skill_name, version_hash, state, content_ref, content_body, created_by, created_at, promoted_at)
		VALUES(?,?,?,?, 'active', ?,?,?,?,?)
		ON CONFLICT(control_tenant_id, skill_key, version_hash) DO UPDATE SET
		 skill_name=excluded.skill_name, state='active', content_ref=excluded.content_ref,
		 content_body=excluded.content_body, promoted_at=COALESCE(skill_versions.promoted_at, excluded.promoted_at)`,
		input.ControlTenantID, input.SkillKey, input.SkillName, input.VersionHash,
		input.ContentRef, input.ContentBody, createdBy, now, now); err != nil {
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
		SkillName: input.SkillName, VersionHash: input.VersionHash, ActivationSource: strings.TrimSpace(input.ActivationSource),
		AttachmentMode: strings.TrimSpace(input.AttachmentMode), State: SkillActivationActive, SelectedAt: time.Unix(now, 0),
	}
	if activation.ActivationSource == "" {
		activation.ActivationSource = "model"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO run_skill_activations
		(id, identity_tenant_id, control_tenant_id, person_id, workspace_id, run_id, sequence,
		 work_unit_id, execution_lane, primary_task_id, related_task_id, skill_key, skill_name,
		 version_hash, activation_source, attachment_mode, state, selected_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, activation.ID, activation.IdentityTenantID,
		activation.ControlTenantID, activation.PersonID, activation.WorkspaceID, activation.RunID,
		activation.Sequence, activation.WorkUnitID, activation.ExecutionLane, activation.PrimaryTaskID,
		activation.RelatedTaskID, activation.SkillKey, activation.SkillName, activation.VersionHash,
		activation.ActivationSource, activation.AttachmentMode, activation.State, now); err != nil {
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
		state, fallback_reason, selected_at, finished_at FROM run_skill_activations
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
		&a.ActivationSource, &a.AttachmentMode, &a.State, &a.FallbackReason, &selected, &finished); err != nil {
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
		state, fallback_reason, selected_at, finished_at FROM run_skill_activations
		WHERE identity_tenant_id=? AND run_id=? AND work_unit_id=? AND execution_lane=?
		AND state IN ('selected','active') LIMIT 1`, normalizeTenant(tenantID), runID, workUnitID, lane)
	activation, err := scanSkillActivation(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return activation, err
}

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
	if signature := strings.TrimSpace(input.FailureSignature); signature != "" && skillFailureGuardCategoryEligible(input.ErrorCategory) {
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

func skillFailureGuardCategoryEligible(category string) bool {
	category = strings.ToLower(strings.TrimSpace(category))
	if category == "" {
		return false
	}
	for _, marker := range []string{"transient", "timeout", "rate_limit", "network", "provider", "environment_drift", "approval", "cancel", "unknown"} {
		if strings.Contains(category, marker) {
			return false
		}
	}
	return true
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
