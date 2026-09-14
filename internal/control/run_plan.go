package control

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const RunRecoveryContractVersion = 1

// completionPreconditionError distinguishes a correctable verdict from a
// storage failure. No completion state was committed when this is returned.
type completionPreconditionError struct{ message string }

func (e *completionPreconditionError) Error() string              { return e.message }
func (*completionPreconditionError) CompletionPrecondition() bool { return true }

type RunPlanStepInput struct {
	StepID               string `json:"step_id,omitempty"`
	Step                 string `json:"step"`
	Status               string `json:"status"`
	SuccessCriteria      string `json:"success_criteria,omitempty"`
	VerificationRequired bool   `json:"verification_required,omitempty"`
	RelatedTaskID        string `json:"related_task_id,omitempty"`
	WorkUnitID           string `json:"work_unit_id,omitempty"`
	WorkUnit             bool   `json:"work_unit,omitempty"`
}

type RunPlanStep struct {
	RunPlanStepInput
	Sequence int `json:"sequence"`
}

type RunPlan struct {
	RunID       string        `json:"run_id"`
	Version     int           `json:"version"`
	Explanation string        `json:"explanation,omitempty"`
	ContentHash string        `json:"content_hash"`
	Steps       []RunPlanStep `json:"steps"`
	CreatedAt   time.Time     `json:"created_at"`
}

type RunPlanProjection struct {
	Plan      RunPlan       `json:"plan"`
	Changed   bool          `json:"changed"`
	WorkUnits []RunWorkUnit `json:"work_units,omitempty"`
	// CriteriaRestated names steps this snapshot marks completed whose
	// success_criteria differs from the bar FIRST declared for them. Restating a
	// criterion can be honest replanning, so this is a fact and not a verdict:
	// a run once moved a step's criterion from "gh api confirms the file exists
	// with the right content" to "a PR is created with the right content" while
	// the step was still pending, completed it snapshots later, and nothing
	// recorded that the bar had moved.
	CriteriaRestated []RunPlanCriteriaChange `json:"criteria_restated,omitempty"`
}

// RunPlanCriteriaChange is one step's acceptance bar before and after.
type RunPlanCriteriaChange struct {
	StepID string `json:"step_id"`
	Step   string `json:"step"`
	From   string `json:"from"`
	To     string `json:"to"`
}

type RunRecoverySnapshot struct {
	ContractVersion    int      `json:"contract_version"`
	PlanVersion        int      `json:"plan_version,omitempty"`
	CurrentPlanStepID  string   `json:"current_plan_step_id,omitempty"`
	UnresolvedStepIDs  []string `json:"unresolved_step_ids,omitempty"`
	UncertainEffectIDs []string `json:"uncertain_effect_ids,omitempty"`
}

// StalePlanStepReferenceError is returned when a model echoes an id not issued
// for the current Run. The tool layer renders the legal ids without parsing
// error prose.
type StalePlanStepReferenceError struct {
	StepID  string
	RunID   string
	Current []string
}

type planVerificationPreconditionError struct{ step string }

func (e *planVerificationPreconditionError) Error() string {
	return fmt.Sprintf("plan step %q requires successful verification before its work unit can complete; the previous plan is unchanged", e.step)
}

func (*planVerificationPreconditionError) PlanVerificationPrecondition() bool { return true }

func (e *StalePlanStepReferenceError) Error() string {
	return fmt.Sprintf("plan step %s does not belong to run %s", e.StepID, e.RunID)
}

func (e *StalePlanStepReferenceError) CurrentPlanStepIDs() []string {
	return append([]string(nil), e.Current...)
}

// SyncRunPlan owns the durable complete-snapshot contract for one Run. It
// resolves server-issued step ids, versions the plan, and updates the coarser
// work-unit attribution in the same transaction.
func (s *Store) SyncRunPlan(ctx context.Context, tenantID, runID, explanation string, input []RunPlanStepInput) (RunPlanProjection, error) {
	if s == nil || s.db == nil {
		return RunPlanProjection{}, fmt.Errorf("control store is unavailable")
	}
	if strings.TrimSpace(runID) == "" || len(input) == 0 {
		return RunPlanProjection{}, fmt.Errorf("run id and at least one plan step are required")
	}
	tenant := normalizeTenant(tenantID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RunPlanProjection{}, err
	}
	defer tx.Rollback()

	var contractVersion int
	if err := tx.QueryRowContext(ctx, `SELECT recovery_contract_version FROM runs WHERE tenant_id=? AND id=?`, tenant, runID).Scan(&contractVersion); err != nil {
		return RunPlanProjection{}, err
	}
	if contractVersion < RunRecoveryContractVersion {
		return RunPlanProjection{}, fmt.Errorf("run %s does not opt into recovery contract v%d", runID, RunRecoveryContractVersion)
	}

	previous, err := latestRunPlanTx(ctx, tx, tenant, runID)
	if err != nil {
		return RunPlanProjection{}, err
	}
	steps, err := resolveRunPlanSteps(runID, input, previous)
	if err != nil {
		return RunPlanProjection{}, err
	}
	workInput, boundaries := projectRunPlanWorkUnits(steps)
	units, err := s.syncRunWorkUnitsTx(ctx, tx, tenant, runID, workInput)
	if err != nil {
		return RunPlanProjection{}, err
	}
	stepWorkUnits := make([]string, len(steps))
	for i, start := range boundaries {
		if i >= len(units) {
			break
		}
		end := len(steps)
		if i+1 < len(boundaries) {
			end = boundaries[i+1]
		}
		for sequence := start; sequence < end; sequence++ {
			stepWorkUnits[sequence] = units[i].ID
		}
		steps[start].WorkUnit = true
		steps[start].WorkUnitID = units[i].ID
	}
	// Completing a work unit freezes its evidence window. Reject a premature
	// close transactionally so a subsequent check can still belong to that unit.
	for i, step := range steps {
		if step.Status != "completed" || !step.VerificationRequired {
			continue
		}
		for _, unit := range units {
			if unit.ID == stepWorkUnits[i] && unit.Status == WorkUnitCompleted && unit.VerificationState != "passed" {
				return RunPlanProjection{}, &planVerificationPreconditionError{step: step.Step}
			}
		}
	}

	original, err := originalPlanCriteriaTx(ctx, tx, tenant, runID)
	if err != nil {
		return RunPlanProjection{}, err
	}
	restated := restatedCompletedCriteria(original, steps)
	hash := hashRunPlanSteps(steps, stepWorkUnits)
	if previous != nil && previous.ContentHash == hash {
		if err := promoteThreadForRunTx(ctx, tx, tenant, runID); err != nil {
			return RunPlanProjection{}, err
		}
		if err := tx.Commit(); err != nil {
			return RunPlanProjection{}, err
		}
		return RunPlanProjection{Plan: *previous, Changed: false, WorkUnits: units}, nil
	}
	version := 1
	if previous != nil {
		version = previous.Version + 1
	}
	now := time.Now()
	if _, err := tx.ExecContext(ctx, `INSERT INTO run_plan_versions
		(run_id, tenant_id, version, explanation, content_hash, created_at) VALUES(?,?,?,?,?,?)`,
		runID, tenant, version, strings.TrimSpace(explanation), hash, now.Unix()); err != nil {
		return RunPlanProjection{}, err
	}
	for i, step := range steps {
		if _, err := tx.ExecContext(ctx, `INSERT INTO run_plan_steps
			(run_id, tenant_id, plan_version, step_id, sequence, step_text, status, success_criteria,
			 verification_required, related_task_id, work_unit_id, work_unit_boundary, created_at)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, runID, tenant, version, step.StepID, i+1,
			step.Step, step.Status, step.SuccessCriteria, boolInt(step.VerificationRequired), step.RelatedTaskID, stepWorkUnits[i],
			boolInt(isRunPlanBoundary(steps, i)), now.Unix()); err != nil {
			return RunPlanProjection{}, err
		}
	}
	// A durable MULTI-STEP Plan is deterministic evidence that this interaction
	// represents ongoing work; one snapshotted step is not, or a one-shot answer
	// that records its lone step would enter the work list. Promotion is part of
	// the same transaction as the plan snapshot, so the work list cannot miss
	// committed planned work. runWorkEvidenceSQL applies the same step bound.
	if len(steps) > 1 {
		if err := promoteThreadForRunTx(ctx, tx, tenant, runID); err != nil {
			return RunPlanProjection{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return RunPlanProjection{}, err
	}
	plan := RunPlan{RunID: runID, Version: version, Explanation: strings.TrimSpace(explanation), ContentHash: hash, Steps: steps, CreatedAt: now}
	return RunPlanProjection{Plan: plan, Changed: true, WorkUnits: units, CriteriaRestated: restated}, nil
}

// originalPlanCriteriaTx returns each step's acceptance bar as first declared
// for this run, keyed by step id.
//
// The comparison baseline has to be the ORIGINAL bar, not the previous
// snapshot's: the run that motivated this moved a step's criterion while the
// step was still pending and completed it several snapshots later, so a
// version-to-version diff saw a status change and nothing else.
func originalPlanCriteriaTx(ctx context.Context, tx *sql.Tx, tenant, runID string) (map[string]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT step_id, success_criteria FROM run_plan_steps
		 WHERE tenant_id=? AND run_id=? ORDER BY plan_version ASC, sequence ASC`, tenant, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	original := map[string]string{}
	for rows.Next() {
		var stepID, criteria string
		if err := rows.Scan(&stepID, &criteria); err != nil {
			return nil, err
		}
		if _, seen := original[stepID]; !seen {
			original[stepID] = criteria
		}
	}
	return original, rows.Err()
}

// restatedCompletedCriteria reports steps that arrive completed under a
// different acceptance bar than the one first declared for them. Only completed
// steps qualify: moving the bar for work still in flight is ordinary
// replanning, while completing against a moved bar is the shape that let a
// false completion resolve a plan cleanly.
func restatedCompletedCriteria(original map[string]string, steps []RunPlanStep) []RunPlanCriteriaChange {
	var changed []RunPlanCriteriaChange
	for _, step := range steps {
		first, ok := original[step.StepID]
		if !ok || step.Status != "completed" {
			continue
		}
		if normalizeRunPlanText(first) == normalizeRunPlanText(step.SuccessCriteria) {
			continue
		}
		changed = append(changed, RunPlanCriteriaChange{
			StepID: step.StepID, Step: step.Step,
			From: first, To: step.SuccessCriteria,
		})
	}
	return changed
}

func resolveRunPlanSteps(runID string, input []RunPlanStepInput, previous *RunPlan) ([]RunPlanStep, error) {
	byID := map[string]RunPlanStep{}
	currentIDs := make([]string, 0)
	if previous != nil {
		for _, step := range previous.Steps {
			byID[step.StepID] = step
			currentIDs = append(currentIDs, step.StepID)
		}
	}
	used := map[string]bool{}
	out := make([]RunPlanStep, 0, len(input))
	for i, item := range input {
		item.StepID = strings.TrimSpace(item.StepID)
		item.Step = strings.TrimSpace(item.Step)
		item.Status = strings.TrimSpace(item.Status)
		item.SuccessCriteria = strings.TrimSpace(item.SuccessCriteria)
		item.RelatedTaskID = strings.TrimSpace(item.RelatedTaskID)
		item.WorkUnitID = strings.TrimSpace(item.WorkUnitID)
		// A first snapshot has no server-issued identities for the model to
		// reference. Some providers still invent descriptive ids despite the
		// schema guidance; treating those strings as stale rejects the entire
		// plan and makes the first visible snapshot arrive only after work has
		// already happened. Normalize both identities to empty on version zero
		// and issue authoritative ids below. Once a plan exists, foreign ids
		// remain a hard stale-precondition error.
		if previous == nil {
			item.StepID = ""
			item.WorkUnitID = ""
		}
		if item.Step == "" {
			return nil, fmt.Errorf("plan[%d].step is required", i)
		}
		if !validRunPlanStatus(item.Status) {
			return nil, fmt.Errorf("plan[%d].status is invalid", i)
		}
		if item.StepID != "" {
			if _, ok := byID[item.StepID]; !ok {
				return nil, &StalePlanStepReferenceError{StepID: item.StepID, RunID: runID, Current: currentIDs}
			}
		} else if previous != nil {
			for _, candidate := range previous.Steps {
				if used[candidate.StepID] || !sameRunPlanIdentity(candidate, item, i == 0) {
					continue
				}
				item.StepID = candidate.StepID
				break
			}
		}
		if item.StepID == "" {
			item.StepID = "step_" + uuid.NewString()
		}
		// Execution attribution is already known for an existing step. A
		// normal progress update need not repeat the work-unit identity.
		if old, ok := byID[item.StepID]; ok && item.WorkUnitID == "" {
			item.WorkUnitID = old.WorkUnitID
			item.WorkUnit = item.WorkUnit || old.WorkUnit
		}
		if used[item.StepID] {
			return nil, fmt.Errorf("plan step %s appears more than once in the snapshot", item.StepID)
		}
		used[item.StepID] = true
		out = append(out, RunPlanStep{RunPlanStepInput: item, Sequence: i + 1})
	}
	// Repeating the current unit's id describes membership, not another
	// independent objective. Keep one boundary and let ordinary steps omit it.
	unitID := ""
	for i := range out {
		if i > 0 && out[i].WorkUnitID != "" && out[i].WorkUnitID == unitID && !out[i].WorkUnit {
			out[i].WorkUnitID = ""
		}
		if isRunPlanBoundary(out, i) {
			unitID = out[i].WorkUnitID
		}
	}
	return out, nil
}

func sameRunPlanIdentity(previous RunPlanStep, next RunPlanStepInput, first bool) bool {
	previousBoundary := previous.WorkUnit
	nextBoundary := first || next.WorkUnit || next.WorkUnitID != "" || next.RelatedTaskID != ""
	return normalizeRunPlanText(previous.Step) == normalizeRunPlanText(next.Step) &&
		previous.RelatedTaskID == next.RelatedTaskID && previousBoundary == nextBoundary
}

func normalizeRunPlanText(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
}

func validRunPlanStatus(status string) bool {
	switch status {
	case "pending", "in_progress", "completed", "cancelled":
		return true
	default:
		return false
	}
}

func projectRunPlanWorkUnits(steps []RunPlanStep) ([]WorkUnitPlanInput, []int) {
	boundaries := []int{0}
	for i := 1; i < len(steps); i++ {
		if isRunPlanBoundary(steps, i) {
			boundaries = append(boundaries, i)
		}
	}
	out := make([]WorkUnitPlanInput, 0, len(boundaries))
	for i, start := range boundaries {
		end := len(steps)
		if i+1 < len(boundaries) {
			end = boundaries[i+1]
		}
		out = append(out, WorkUnitPlanInput{
			WorkUnitID: steps[start].WorkUnitID, GoalDigest: steps[start].Step,
			PlanStatus: aggregateRunPlanStatus(steps[start:end]), RelatedTaskID: steps[start].RelatedTaskID,
		})
	}
	return out, boundaries
}

func isRunPlanBoundary(steps []RunPlanStep, index int) bool {
	if index == 0 {
		return true
	}
	step := steps[index]
	return step.WorkUnit || step.WorkUnitID != "" || step.RelatedTaskID != ""
}

func aggregateRunPlanStatus(steps []RunPlanStep) string {
	hasPending, hasCompleted := false, false
	for _, step := range steps {
		switch step.Status {
		case "in_progress":
			return "in_progress"
		case "pending":
			hasPending = true
		case "completed":
			hasCompleted = true
		}
	}
	if hasPending {
		return "pending"
	}
	if hasCompleted {
		return "completed"
	}
	return "cancelled"
}

func hashRunPlanSteps(steps []RunPlanStep, workUnitIDs []string) string {
	type hashStep struct {
		StepID, Step, Status, SuccessCriteria, RelatedTaskID, WorkUnitID string
		VerificationRequired, WorkUnit                                   bool
	}
	canonical := make([]hashStep, 0, len(steps))
	for i, step := range steps {
		workUnitID := ""
		if i < len(workUnitIDs) {
			workUnitID = workUnitIDs[i]
		}
		canonical = append(canonical, hashStep{step.StepID, step.Step, step.Status, step.SuccessCriteria, step.RelatedTaskID, workUnitID, step.VerificationRequired, step.WorkUnit})
	}
	data, _ := json.Marshal(canonical)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

func latestRunPlanTx(ctx context.Context, tx *sql.Tx, tenantID, runID string) (*RunPlan, error) {
	row := tx.QueryRowContext(ctx, `SELECT version, explanation, content_hash, created_at
		FROM run_plan_versions WHERE tenant_id=? AND run_id=? ORDER BY version DESC LIMIT 1`, tenantID, runID)
	var plan RunPlan
	var created int64
	if err := row.Scan(&plan.Version, &plan.Explanation, &plan.ContentHash, &created); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	plan.RunID = runID
	plan.CreatedAt = time.Unix(created, 0)
	rows, err := tx.QueryContext(ctx, `SELECT step_id, sequence, step_text, status, success_criteria, verification_required,
		related_task_id, work_unit_id, work_unit_boundary FROM run_plan_steps
		WHERE tenant_id=? AND run_id=? AND plan_version=? ORDER BY sequence`, tenantID, runID, plan.Version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var step RunPlanStep
		var workUnitID string
		var verificationRequired, boundary int
		if err := rows.Scan(&step.StepID, &step.Sequence, &step.Step, &step.Status, &step.SuccessCriteria, &verificationRequired,
			&step.RelatedTaskID, &workUnitID, &boundary); err != nil {
			return nil, err
		}
		step.VerificationRequired = verificationRequired != 0
		step.WorkUnit = boundary != 0
		if step.WorkUnit {
			step.WorkUnitID = workUnitID
		}
		plan.Steps = append(plan.Steps, step)
	}
	return &plan, rows.Err()
}

func (s *Store) LatestRunPlan(ctx context.Context, tenantID, runID string) (*RunPlan, error) {
	if s == nil || s.db == nil || strings.TrimSpace(runID) == "" {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	plan, err := latestRunPlanTx(ctx, tx, normalizeTenant(tenantID), runID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return plan, nil
}

func (s *Store) ValidateRunCompletion(ctx context.Context, tenantID, runID string) error {
	if s == nil || s.db == nil || strings.TrimSpace(runID) == "" {
		return nil
	}
	var contractVersion int
	if err := s.db.QueryRowContext(ctx, `SELECT recovery_contract_version FROM runs WHERE tenant_id=? AND id=?`, normalizeTenant(tenantID), runID).Scan(&contractVersion); err != nil {
		return err
	}
	if contractVersion < RunRecoveryContractVersion {
		return nil
	}
	plan, err := s.LatestRunPlan(ctx, tenantID, runID)
	if err != nil {
		return err
	}
	var unresolved []string
	if plan != nil {
		for _, step := range plan.Steps {
			if step.Status != "completed" && step.Status != "cancelled" {
				unresolved = append(unresolved, step.Step)
			}
		}
	}
	if len(unresolved) > 0 {
		return &completionPreconditionError{fmt.Sprintf("successful run still has unresolved durable plan steps: %s", strings.Join(unresolved, "; "))}
	}
	var unverified int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM run_plan_steps p
		LEFT JOIN run_work_units w ON w.run_id=p.run_id AND w.id=p.work_unit_id
		WHERE p.tenant_id=? AND p.run_id=?
		  AND p.plan_version=(SELECT MAX(version) FROM run_plan_versions WHERE tenant_id=? AND run_id=?)
		  AND p.status='completed' AND p.verification_required=1
		  AND COALESCE(w.verification_state,'')<>'passed'`, normalizeTenant(tenantID), runID,
		normalizeTenant(tenantID), runID).Scan(&unverified); err != nil {
		return err
	}
	if unverified > 0 {
		return &completionPreconditionError{fmt.Sprintf("successful run still has %d completed plan step(s) without required verification evidence", unverified)}
	}
	var uncertain int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tool_ledger
		WHERE tenant_id=? AND run_id=? AND status IN (?, ?) AND retry_class=? AND verification_state<>'verified'`,
		normalizeTenant(tenantID), runID, ToolLedgerStarted, ToolLedgerDispatched, "side_effect").Scan(&uncertain); err != nil {
		return err
	}
	if uncertain > 0 {
		return &completionPreconditionError{fmt.Sprintf("successful run still has %d uncertain side effect(s); verify their current state before finishing", uncertain)}
	}
	return nil
}

func (s *Store) RunRecoveryState(ctx context.Context, tenantID, runID string) (RunRecoverySnapshot, error) {
	snapshot := RunRecoverySnapshot{}
	if s == nil || s.db == nil || strings.TrimSpace(runID) == "" {
		return snapshot, nil
	}
	if err := s.db.QueryRowContext(ctx, `SELECT recovery_contract_version FROM runs WHERE tenant_id=? AND id=?`,
		normalizeTenant(tenantID), runID).Scan(&snapshot.ContractVersion); err != nil {
		return snapshot, err
	}
	if snapshot.ContractVersion < RunRecoveryContractVersion {
		return snapshot, nil
	}
	plan, err := s.LatestRunPlan(ctx, tenantID, runID)
	if err != nil {
		return snapshot, err
	}
	if plan != nil {
		snapshot.PlanVersion = plan.Version
		for _, step := range plan.Steps {
			if step.Status == "in_progress" {
				snapshot.CurrentPlanStepID = step.StepID
			}
			if step.Status != "completed" && step.Status != "cancelled" {
				snapshot.UnresolvedStepIDs = append(snapshot.UnresolvedStepIDs, step.StepID)
			}
		}
	}
	entries, err := s.ListUncertainToolEntries(ctx, tenantID, runID, 100)
	if err != nil {
		return snapshot, err
	}
	for _, entry := range entries {
		if entry.EffectID != "" {
			snapshot.UncertainEffectIDs = append(snapshot.UncertainEffectIDs, entry.EffectID)
		}
	}
	return snapshot, nil
}
