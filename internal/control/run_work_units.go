package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

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

// StaleWorkUnitReferenceError preserves the local rejected id while exposing
// the current run's legal ids to the tool layer for a stable recovery message.
// It is intentionally typed: callers must not parse error prose to recover.
type StaleWorkUnitReferenceError struct {
	WorkUnitID string
	RunID      string
	Current    []string
}

func (e *StaleWorkUnitReferenceError) Error() string {
	return fmt.Sprintf("work unit %s does not belong to run %s", e.WorkUnitID, e.RunID)
}

func (e *StaleWorkUnitReferenceError) CurrentWorkUnitIDs() []string {
	return append([]string(nil), e.Current...)
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
		unit, err := resolvePlanWorkUnit(runID, item, existing, byID, used)
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

func resolvePlanWorkUnit(runID string, item WorkUnitPlanInput, existing []RunWorkUnit, byID map[string]*RunWorkUnit, used map[string]bool) (*RunWorkUnit, error) {
	if item.WorkUnitID != "" {
		unit := byID[item.WorkUnitID]
		if unit == nil {
			current := make([]string, 0, len(existing))
			for _, candidate := range existing {
				current = append(current, candidate.ID)
			}
			return nil, &StaleWorkUnitReferenceError{WorkUnitID: item.WorkUnitID, RunID: runID, Current: current}
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
	if err != nil {
		return err
	}
	if workUnitTerminal(status) {
		_, err = tx.ExecContext(ctx, `DELETE FROM skill_candidate_refs
			WHERE identity_tenant_id=? AND run_id=? AND work_unit_id=?`,
			unit.IdentityTenantID, unit.RunID, unit.ID)
	}
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
