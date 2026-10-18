package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"selfmind/internal/verification"
)

// RunCompletionInput is a proposed business result. ExpectedPlanVersion is
// supplied by the execution cursor, not by the model. The candidate is not a
// durable state or an authorization token.
type RunCompletionInput struct {
	ExpectedPlanVersion int
	Status              string
	PlanPatch           []RunPlanStepInput
	Assessments         []verification.Assessment
}

// RunCompletionCandidate is immutable outside control. Materialization runs
// the same validation again; a successful preview never authorizes a stale
// finish. No caller can replace its identity, scope or frozen proposal.
type RunCompletionCandidate struct {
	tenant, person, runID string
	input                 RunCompletionInput
	lifetime              context.Context
	Conditions            []verification.Assessment
	Assessment            verification.AssessmentProjection
	ObservationChecks     []verification.Check
	Mutations             []verification.Mutation
	receipt               *Event
}

// CommittedReceipt returns the exact persisted result and accepted Plan, never
// a post-commit re-read that could fail or observe another version.
func (c *RunCompletionCandidate) CommittedReceipt() (Event, bool) {
	if c.receipt == nil {
		return Event{}, false
	}
	event := *c.receipt
	event.Payload = append(json.RawMessage(nil), event.Payload...)
	return event, true
}

// PrepareRunCompletion uses only transaction-local implementations. The
// provisional Plan/work-unit rows are always rolled back, including on success;
// public Plan projection, Skill activation and event publication are not called.
func (s *Store) PrepareRunCompletion(ctx context.Context, tenant, person, runID string, input RunCompletionInput) (*RunCompletionCandidate, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	var frozen RunCompletionInput
	if err = json.Unmarshal(encoded, &frozen); err != nil {
		return nil, err
	}
	candidate := &RunCompletionCandidate{tenant: normalizeTenant(tenant), person: person, runID: runID, input: frozen, lifetime: ctx}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	conditions, err := s.applyRunCompletionTx(ctx, tx, candidate)
	if err != nil {
		return nil, err
	}
	candidate.Conditions = conditions.Conditions
	candidate.Assessment = conditions
	plan, err := latestRunPlanTx(ctx, tx, candidate.tenant, runID)
	if err != nil {
		return nil, err
	}
	checks, mutations, _, err := conditionEvidenceTx(ctx, tx, candidate.tenant, runID, plan)
	if err != nil {
		return nil, err
	}
	for _, check := range checks {
		if check.Observation != nil {
			candidate.ObservationChecks = append(candidate.ObservationChecks, check)
		}
	}
	candidate.Mutations = mutations
	return candidate, nil
}

func (s *Store) applyRunCompletionTx(ctx context.Context, tx *sql.Tx, candidate *RunCompletionCandidate) (verification.AssessmentProjection, error) {
	if err := candidate.lifetime.Err(); err != nil {
		return verification.AssessmentProjection{}, err
	}
	var status, person string
	var cancelled bool
	if err := tx.QueryRowContext(ctx, `SELECT person_id,status,COALESCE(cancel_requested,0) FROM runs
		WHERE tenant_id=? AND id=?`, candidate.tenant, candidate.runID).Scan(&person, &status, &cancelled); err != nil {
		return verification.AssessmentProjection{}, err
	}
	if person != candidate.person || person == "" {
		return verification.AssessmentProjection{}, fmt.Errorf("completion identity does not own Run")
	}
	if cancelled || status == "cancelled" {
		return verification.AssessmentProjection{}, ErrRunCancellationRequested
	}
	if status != "running" {
		return verification.AssessmentProjection{}, &completionPreconditionError{"Run is no longer executing; inspect its current result before finishing"}
	}
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM steering_mailbox
		WHERE tenant_id=? AND person_id=? AND run_id=? AND status IN ('accepted','claimed','deferred')`,
		candidate.tenant, person, candidate.runID).Scan(&pending); err != nil {
		return verification.AssessmentProjection{}, err
	}
	if pending > 0 {
		return verification.AssessmentProjection{}, &completionContextChangedError{"New user input must be considered before finishing"}
	}
	plan, err := latestRunPlanTx(ctx, tx, candidate.tenant, candidate.runID)
	if err != nil {
		return verification.AssessmentProjection{}, err
	}
	version := 0
	if plan != nil {
		version = plan.Version
	}
	if version != candidate.input.ExpectedPlanVersion {
		return verification.AssessmentProjection{}, &completionContextChangedError{"Accepted Plan changed before completion; reconsider its current state"}
	}
	if err = validateFinalPlanPatch(plan, candidate.input.PlanPatch); err != nil {
		return verification.AssessmentProjection{}, &conditionAssessmentInputError{err}
	}
	if err = adoptCompletionObservationsTx(ctx, tx, candidate.tenant, person, candidate.runID, plan, candidate.input.Assessments); err != nil {
		return verification.AssessmentProjection{}, err
	}
	var finalProjection RunPlanProjection
	if len(candidate.input.PlanPatch) > 0 {
		finalProjection, err = s.patchRunPlanTx(ctx, tx, candidate.tenant, candidate.runID, version, "Final progress", candidate.input.PlanPatch)
		if err != nil {
			return verification.AssessmentProjection{}, err
		}
	}
	condition, err := resolveConditionAssessmentsTx(ctx, tx, candidate.tenant, candidate.runID, candidate.input.Assessments)
	if err != nil {
		return verification.AssessmentProjection{}, err
	}
	if candidate.input.Status == "done" {
		if err = completionVerificationDeferrals(finalProjection); err != nil {
			return verification.AssessmentProjection{}, err
		}
		if err = validateRunCompletionTx(ctx, tx, candidate.tenant, candidate.runID, candidate.input.Assessments); err != nil {
			return verification.AssessmentProjection{}, err
		}
	}
	return condition, nil
}

func validateFinalPlanPatch(plan *RunPlan, patch []RunPlanStepInput) error {
	if len(patch) == 0 {
		return nil
	}
	if plan == nil {
		return fmt.Errorf("create the visible Plan before submitting its final changes")
	}
	current := map[string]RunPlanStep{}
	for _, step := range plan.Steps {
		current[step.StepID] = step
	}
	for _, change := range patch {
		old, ok := current[change.StepID]
		if !ok {
			return fmt.Errorf("final Plan patch step %q is not current", change.StepID)
		}
		if change.Step != "" && change.Step != old.Step || change.SuccessCriteria != "" && change.SuccessCriteria != old.SuccessCriteria ||
			change.VerificationRequired && !old.VerificationRequired || change.WorkUnitID != "" && change.WorkUnitID != old.WorkUnitID || change.WorkUnit && !old.WorkUnit {
			return fmt.Errorf("final Plan patch preserves obligations and scope; use update_plan to revise step %q before assessing completion", change.StepID)
		}
	}
	_, err := mergeRunPlanPatch("", plan, patch)
	return err
}

type completionContextChangedError struct{ message string }

func (e *completionContextChangedError) Error() string                { return e.message }
func (*completionContextChangedError) CompletionContextChanged() bool { return true }

func (s *Store) materializeCompletionTx(ctx context.Context, tx *sql.Tx, tenant, person string, input *RunFinalization) error {
	candidate := input.Completion
	if candidate == nil {
		return nil
	}
	if candidate.tenant != tenant || candidate.person != person || candidate.runID != input.RunID || candidate.input.Status != input.RunStatus {
		return fmt.Errorf("completion candidate does not match finalization owner or outcome")
	}
	conditions, err := s.applyRunCompletionTx(ctx, tx, candidate)
	if err != nil {
		return err
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(input.Event.Payload, &payload); err != nil {
		return err
	}
	var outcome map[string]interface{}
	if err := json.Unmarshal(payload["outcome"], &outcome); err != nil {
		return err
	}
	if len(conditions.Conditions) > 0 {
		outcome["assessments"] = conditions.Conditions
	}
	if len(candidate.input.PlanPatch) > 0 {
		plan, err := latestRunPlanTx(ctx, tx, tenant, input.RunID)
		if err != nil {
			return err
		}
		payload["accepted_plan"], err = json.Marshal(plan)
		if err != nil {
			return err
		}
	}
	payload["outcome"], err = json.Marshal(outcome)
	if err != nil {
		return err
	}
	input.Event.Payload, err = json.Marshal(payload)
	return err
}
