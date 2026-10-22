package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"selfmind/internal/verification"
)

// InheritedPlanEvidence is historical evidence for Main to assess, not a
// child-Run verification verdict. External conditions and unrecorded changes
// can make an earlier successful observation insufficient today.
type InheritedPlanEvidence struct {
	StepID             string                   `json:"step_id"`
	SourceRunID        string                   `json:"source_run_id"`
	SourceStepID       string                   `json:"source_step_id"`
	SourcePlanVersion  int                      `json:"source_plan_version"`
	SourceStatus       string                   `json:"source_status"`
	SourceCriterion    string                   `json:"source_criterion"`
	CriterionChanged   bool                     `json:"criterion_changed"`
	PriorVerification  string                   `json:"prior_verification"`
	LatestCheck        string                   `json:"latest_check"`
	Target             string                   `json:"target"`
	CheckedAt          time.Time                `json:"checked_at"`
	CheckID            string                   `json:"check_id"`
	ObservedCriterion  string                   `json:"observed_criterion"`
	ReuseBlockedReason string                   `json:"reuse_blocked_reason"`
	PriorCondition     *verification.Assessment `json:"prior_condition,omitempty"`
}

// ListInheritedPlanEvidence follows only the child Run's exact parent edge and
// runtime-imported step ids. It never matches steps by sequence or prose and
// never writes a new evidence event or changes the child's verification state.
func (s *Store) ListInheritedPlanEvidence(ctx context.Context, tenantID, childRunID string) ([]InheritedPlanEvidence, error) {
	if s == nil || s.db == nil || strings.TrimSpace(childRunID) == "" {
		return nil, nil
	}
	tenant := normalizeTenant(tenantID)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := inheritedPlanEvidenceTx(ctx, tx, tenant, childRunID)
	if err != nil {
		return nil, err
	}
	if err := attachInheritedConditionsTx(ctx, tx, result); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func inheritedPlanEvidenceTx(ctx context.Context, tx *sql.Tx, tenant, childRunID string) ([]InheritedPlanEvidence, error) {
	var parentRunID, childPersonID, parentPersonID string
	if err := tx.QueryRowContext(ctx, `SELECT resumes_run_id, person_id FROM runs WHERE tenant_id=? AND id=?`, tenant, childRunID).
		Scan(&parentRunID, &childPersonID); err != nil {
		return nil, err
	}
	if parentRunID == "" {
		return nil, nil
	}
	if err := tx.QueryRowContext(ctx, `SELECT person_id FROM runs WHERE tenant_id=? AND id=?`, tenant, parentRunID).
		Scan(&parentPersonID); err != nil {
		return nil, err
	}
	if childPersonID != parentPersonID {
		return nil, nil
	}
	childPlan, err := latestRunPlanTx(ctx, tx, tenant, childRunID)
	if err != nil || childPlan == nil {
		return nil, err
	}
	result := make([]InheritedPlanEvidence, 0, len(childPlan.Steps))
	for _, step := range childPlan.Steps {
		if step.SourceStepID == "" || step.SourcePlanVersion == 0 {
			continue
		}
		entry := InheritedPlanEvidence{
			StepID: step.StepID, SourceRunID: parentRunID, SourceStepID: step.SourceStepID,
		}
		if cached, ok, err := inheritedEvidenceReferenceTx(ctx, tx, childRunID, parentRunID, step.StepID); err != nil {
			return nil, err
		} else if ok {
			cached.CriterionChanged = cached.CriterionChanged || normalizeRunPlanText(cached.SourceCriterion) != normalizeRunPlanText(step.SuccessCriteria)
			if err := inheritedEvidenceChangesTx(ctx, tx, childRunID, &cached); err != nil {
				return nil, err
			}
			result = append(result, cached)
			continue
		}
		sourceRunID, sourceStepID, sourceVersion := parentRunID, step.SourceStepID, step.SourcePlanVersion
		for depth := 0; depth < 8 && sourceStepID != "" && sourceVersion > 0; depth++ {
			var sourceStatus, sourceCriterion, sourceWorkUnitID, nextStepID string
			var nextVersion int
			err := tx.QueryRowContext(ctx, `SELECT status, success_criteria, work_unit_id, source_step_id, source_plan_version FROM run_plan_steps
				WHERE tenant_id=? AND run_id=? AND plan_version=? AND step_id=?`,
				tenant, sourceRunID, sourceVersion, sourceStepID).
				Scan(&sourceStatus, &sourceCriterion, &sourceWorkUnitID, &nextStepID, &nextVersion)
			if err == sql.ErrNoRows {
				break
			}
			if err != nil {
				return nil, err
			}
			entry.SourceRunID, entry.SourceStepID = sourceRunID, sourceStepID
			entry.SourcePlanVersion = sourceVersion
			entry.SourceStatus, entry.SourceCriterion = sourceStatus, sourceCriterion
			entry.CriterionChanged = entry.CriterionChanged || normalizeRunPlanText(sourceCriterion) != normalizeRunPlanText(step.SuccessCriteria)
			entry.PriorVerification = ""
			if sourceWorkUnitID != "" {
				err = tx.QueryRowContext(ctx, `SELECT verification_state FROM run_work_units
					WHERE identity_tenant_id=? AND run_id=? AND id=?`,
					tenant, sourceRunID, sourceWorkUnitID).Scan(&entry.PriorVerification)
				if err != nil && err != sql.ErrNoRows {
					return nil, err
				}
			}
			state, summary, checks, err := sourceStepVerificationTx(ctx, tx, sourceRunID, sourceStepID, sourceWorkUnitID)
			if err != nil {
				return nil, err
			}
			if state == "blocked" && len(checks) == 0 {
				entry.PriorVerification, entry.ReuseBlockedReason = state, summary
				break
			}
			if len(checks) > 0 {
				entry.PriorVerification = state
				// Keep the exact effective observation that the frozen window used.
				check := checks[0]
				for _, candidate := range checks[1:] {
					if candidate.FinishedAt >= check.FinishedAt {
						check = candidate
					}
				}
				entry.CheckID, entry.ObservedCriterion = check.ToolCallID, check.Binding.Criterion
				entry.LatestCheck, entry.Target = check.Status, check.Binding.Target
				entry.CheckedAt = time.Unix(0, check.FinishedAt)
				break
			}
			// New continuations carry a direct source reference in the existing
			// event journal. Repeated mechanical waits therefore do not lengthen
			// the proof lookup or expand Main's history window.
			if cached, ok, err := inheritedEvidenceReferenceTx(ctx, tx, sourceRunID, "", sourceStepID); err != nil {
				return nil, err
			} else if ok && cached.CheckID != "" {
				cached.StepID = step.StepID
				cached.CriterionChanged = cached.CriterionChanged || entry.CriterionChanged
				if err := inheritedEvidenceChangesTx(ctx, tx, sourceRunID, &cached); err != nil {
					return nil, err
				}
				entry = cached
				break
			}
			if nextStepID == "" || nextVersion == 0 {
				break
			}
			var nextRunID, nextPersonID string
			if err := tx.QueryRowContext(ctx, `SELECT resumes_run_id, person_id FROM runs WHERE tenant_id=? AND id=?`, tenant, sourceRunID).
				Scan(&nextRunID, &nextPersonID); err != nil {
				return nil, err
			}
			if nextRunID == "" || nextRunID == sourceRunID || nextPersonID != childPersonID {
				break
			}
			var nextParentPersonID string
			if err := tx.QueryRowContext(ctx, `SELECT person_id FROM runs WHERE tenant_id=? AND id=?`, tenant, nextRunID).
				Scan(&nextParentPersonID); err == sql.ErrNoRows {
				break
			} else if err != nil {
				return nil, err
			}
			if nextParentPersonID != childPersonID {
				break
			}
			sourceRunID, sourceStepID, sourceVersion = nextRunID, nextStepID, nextVersion
		}
		if entry.SourceStatus != "" && (step.VerificationRequired || entry.CheckID != "") {
			result = append(result, entry)
		}
	}
	return result, nil
}

type inheritedEvidenceReference struct {
	Version           int                     `json:"version"`
	ResumeSourceRunID string                  `json:"resumes_run_id"`
	Items             []InheritedPlanEvidence `json:"items"`
}

func inheritedEvidenceReferenceTx(ctx context.Context, tx *sql.Tx, runID, parentRunID, stepID string) (InheritedPlanEvidence, bool, error) {
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT payload_json FROM task_events WHERE run_id=? AND type='plan.evidence_inherited' ORDER BY cursor DESC LIMIT 1`, runID).Scan(&raw)
	if err == sql.ErrNoRows {
		return InheritedPlanEvidence{}, false, nil
	}
	if err != nil {
		return InheritedPlanEvidence{}, false, err
	}
	var ref inheritedEvidenceReference
	if err := json.Unmarshal([]byte(raw), &ref); err != nil {
		return InheritedPlanEvidence{}, false, err
	}
	if parentRunID == "" {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(resumes_run_id,'') FROM runs WHERE id=?`, runID).Scan(&parentRunID); err != nil {
			return InheritedPlanEvidence{}, false, err
		}
	}
	if ref.Version != 1 || ref.ResumeSourceRunID != parentRunID {
		return InheritedPlanEvidence{}, false, nil
	}
	for _, item := range ref.Items {
		if item.StepID == stepID {
			return item, true, nil
		}
	}
	return InheritedPlanEvidence{}, false, nil
}

// sourceStepVerificationTx reuses the work-unit projection, including legacy
// binding normalization and every effective obligation, instead of reading the
// latest event of an entire Run. An open window also preserves a saved check
// when execution stopped before the Plan's close-out transaction.
func sourceStepVerificationTx(ctx context.Context, tx *sql.Tx, runID, stepID, workUnitID string) (string, string, []verification.Check, error) {
	start, end, err := workUnitEvidenceWindowTx(ctx, tx, runID, workUnitID)
	if err != nil {
		if err == sql.ErrNoRows {
			return "blocked", "Source work-unit window is unavailable.", nil, nil
		}
		return "", "", nil, err
	}
	mutations, checks, err := workUnitVerificationChecksTx(ctx, tx, runID, start, end, stepID)
	if err != nil {
		return "blocked", err.Error(), nil, nil
	}
	state, summary := verification.StateWithMutations(mutations, checks)
	return state, summary, verification.Latest(checks), nil
}
