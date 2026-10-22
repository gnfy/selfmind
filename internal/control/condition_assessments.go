package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"selfmind/internal/verification"
)

type conditionAssessmentInputError struct{ error }

func (*conditionAssessmentInputError) ConditionAssessmentInput() bool { return true }
func (e *conditionAssessmentInputError) Unwrap() error                { return e.error }

type conditionAssessmentEvidenceError struct{ error }

func (*conditionAssessmentEvidenceError) CompletionPrecondition() bool { return true }
func (e *conditionAssessmentEvidenceError) Unwrap() error              { return e.error }

// ResolveConditionAssessments shares the Plan's evidence windows and dependency
// rules. It performs no write; finish_run stores the accepted judgment with its
// outcome through the existing durable event path.
func (s *Store) ResolveConditionAssessments(ctx context.Context, tenant, runID string, input []verification.Assessment) (verification.AssessmentProjection, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return verification.AssessmentProjection{}, err
	}
	defer tx.Rollback()
	out, err := resolveConditionAssessmentsTx(ctx, tx, normalizeTenant(tenant), runID, input)
	if err != nil {
		return out, err
	}
	return out, tx.Commit()
}

func recordedConditionAssessmentsTx(ctx context.Context, tx *sql.Tx, runID string) ([]verification.Assessment, error) {
	rows, err := tx.QueryContext(ctx, `SELECT payload_json FROM task_events WHERE run_id=? AND type='run.outcome'
		AND json_valid(payload_json) AND json_type(payload_json,'$.assessments')='array'
		AND json_array_length(payload_json,'$.assessments')>0 ORDER BY cursor`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var recorded []verification.Assessment
	positions := map[string]int{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var outcome struct {
			Assessments []verification.Assessment `json:"assessments"`
		}
		if err := json.Unmarshal([]byte(raw), &outcome); err != nil {
			return nil, fmt.Errorf("recorded condition assessments are unreadable: %w", err)
		}
		for _, assessment := range outcome.Assessments {
			if (assessment.Version != 1 && assessment.Version != 2) || assessment.RunID != runID {
				return nil, fmt.Errorf("recorded condition assessment has no supported runtime provenance")
			}
			key := assessmentIdentity(assessment)
			if index, exists := positions[key]; exists {
				recorded[index] = assessment
			} else {
				if len(recorded) >= 128 {
					return nil, fmt.Errorf("condition history exceeds the bounded review limit")
				}
				positions[key] = len(recorded)
				recorded = append(recorded, assessment)
			}
		}
	}
	return recorded, rows.Err()
}

func assessmentIdentity(a verification.Assessment) string {
	raw, _ := json.Marshal([]interface{}{a.RunID, a.StepID, a.PlanCriterion, a.Criterion, a.Target, a.CWD})
	return string(raw)
}

// Nil input reads the exact Run's last accepted judgment. An explicit new
// judgment is validated against the same snapshot without first persisting it.
func resolveConditionAssessmentsTx(ctx context.Context, tx *sql.Tx, tenant, runID string, input []verification.Assessment) (verification.AssessmentProjection, error) {
	var person string
	if err := tx.QueryRowContext(ctx, `SELECT person_id FROM runs WHERE tenant_id=? AND id=?`, tenant, runID).Scan(&person); err != nil {
		return verification.AssessmentProjection{}, err
	}
	recorded, err := recordedConditionAssessmentsTx(ctx, tx, runID)
	if err != nil {
		return verification.AssessmentProjection{}, err
	}
	if len(input) == 0 && len(recorded) == 0 {
		return verification.AssessmentProjection{State: "unassessed"}, nil
	}
	plan, err := latestRunPlanTx(ctx, tx, tenant, runID)
	if err != nil {
		return verification.AssessmentProjection{}, err
	}
	checks, mutations, requirements, err := conditionEvidenceTx(ctx, tx, tenant, runID, plan)
	if err != nil {
		return verification.AssessmentProjection{}, err
	}
	checks, err = stepAssessmentChecksTx(ctx, tx, tenant, runID, plan, checks, append(append([]verification.Assessment(nil), input...), recorded...))
	if err != nil {
		return verification.AssessmentProjection{}, err
	}
	prepared, err := verification.ProjectAssessments(runID, input, checks, mutations, requirements)
	if err != nil {
		explainUnavailableEvidence(ctx, tx, tenant, runID, err)
		var issues *verification.EvidenceIssuesError
		if errors.As(err, &issues) {
			return prepared, &conditionAssessmentEvidenceError{err}
		}
		return prepared, &conditionAssessmentInputError{err}
	}
	// New findings replace only the same obligation. An unrelated successful
	// check cannot erase an earlier negative finding. Replanned/retired steps
	// remain in history; a changed current criterion needs explicit reassessment.
	merged := append([]verification.Assessment(nil), prepared.Conditions...)
	for _, old := range recorded {
		if old.StepID != "" {
			if _, present := requirements[old.StepID]; !present {
				continue
			}
		}
		replaced := false
		for _, next := range prepared.Conditions {
			if assessmentIdentity(old) == assessmentIdentity(next) ||
				old.StepID != "" && old.StepID == next.StepID && old.PlanCriterion != next.PlanCriterion {
				replaced = true
				break
			}
		}
		if !replaced {
			merged = append(merged, old)
		}
	}
	projected, err := verification.ProjectAssessments(runID, merged, checks, mutations, requirements)
	if err != nil {
		explainUnavailableEvidence(ctx, tx, tenant, runID, err)
		var issues *verification.EvidenceIssuesError
		var unavailable *verification.EvidenceUnavailableError
		if errors.As(err, &issues) || errors.As(err, &unavailable) {
			return projected, &conditionAssessmentEvidenceError{err}
		}
		return projected, &completionPreconditionError{err.Error()}
	}
	for _, blocker := range prepared.Blockers {
		found := false
		for _, existing := range projected.Blockers {
			if existing == blocker {
				found = true
				break
			}
		}
		if !found {
			projected.Blockers = append(projected.Blockers, blocker)
		}
	}
	if len(projected.Blockers) > 0 {
		projected.ReviewComplete = false
	}
	if projected.State == "unassessed" || prepared.State == "not_met" || (prepared.State == "unknown" && projected.State != "not_met") {
		projected.State = prepared.State
	}
	return projected, nil
}

// explainUnavailableEvidence adds what the Run's Plan knows about a cited id the
// Run's own checks do not contain; see earlierRunEvidenceNoteTx.
func explainUnavailableEvidence(ctx context.Context, tx *sql.Tx, tenant, runID string, err error) {
	var unavailable *verification.EvidenceUnavailableError
	if errors.As(err, &unavailable) {
		unavailable.Note = earlierRunEvidenceNoteTx(ctx, tx, tenant, runID, unavailable.Ref)
	}
}

// conditionEvidenceTx is the single selector for prospective receipts and
// accepted assessments. Both see the same Plan/work-unit windows and mutations.
func conditionEvidenceTx(ctx context.Context, tx *sql.Tx, tenant, runID string, plan *RunPlan) ([]verification.Check, []verification.Mutation, map[string]verification.AssessmentRequirement, error) {
	last, err := maxRunEventCursorTx(ctx, tx, runID)
	if err != nil {
		return nil, nil, nil, err
	}
	mutations, allChecks, err := workUnitVerificationChecksTx(ctx, tx, runID, 0, last)
	if err != nil {
		return nil, nil, nil, err
	}
	requirements := map[string]verification.AssessmentRequirement{}
	var checks []verification.Check
	for _, check := range allChecks {
		if check.Binding == nil || check.Binding.StepID == "" {
			checks = append(checks, check)
		}
	}
	if plan != nil {
		unitID := ""
		for _, step := range plan.Steps {
			if step.WorkUnitID != "" {
				unitID = step.WorkUnitID
			}
			requirements[step.StepID] = verification.AssessmentRequirement{Criterion: step.SuccessCriteria, PositiveRequired: step.VerificationRequired}
			start, end, err := workUnitEvidenceWindowTx(ctx, tx, runID, unitID)
			if err != nil {
				return nil, nil, nil, err
			}
			_, bound, err := workUnitVerificationChecksTx(ctx, tx, runID, start, end, step.StepID)
			if err != nil {
				return nil, nil, nil, err
			}
			checks = append(checks, bound...)
		}
	}
	return checks, mutations, requirements, nil
}
