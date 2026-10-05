package control

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

type planCancellationPreconditionError struct{ message string }

func (e *planCancellationPreconditionError) Error() string                    { return e.message }
func (*planCancellationPreconditionError) PlanCancellationPrecondition() bool { return true }

func assessedCancellation(step RunPlanStepInput) bool {
	return strings.TrimSpace(step.CancellationReason) != "" && (step.CancellationDisposition == "not_required" || (step.CancellationDisposition == "user_takeover" && strings.TrimSpace(step.UserTakeoverQuote) != ""))
}

func assessedCancellationForContract(step RunPlanStepInput, version int) bool {
	if !assessedCancellation(step) {
		return false
	}
	return version < scopeEvidencePlanContractVersion || step.CancellationDisposition != "not_required" ||
		step.ReplacementStepID != "" || step.ScopeChangeQuote != ""
}

func retainOmittedOpenSteps(steps []RunPlanStep, previous *RunPlan) ([]RunPlanStep, []RunPlanStep) {
	if previous == nil {
		return steps, nil
	}
	seen := map[string]bool{}
	for _, step := range steps {
		seen[step.StepID] = true
	}
	var retained []RunPlanStep
	for _, old := range previous.Steps {
		if seen[old.StepID] || old.Status == "completed" || old.Status == "cancelled" {
			continue
		}
		// Retain the exact obligation, without creating a second active step.
		old.Status, old.Sequence = "pending", len(steps)+1
		// The current snapshot owns coarse work-unit boundaries. Retaining a
		// missing obligation is not a second declaration of its previous unit;
		// the stable step id and earlier snapshots preserve that attribution.
		old.WorkUnitID, old.WorkUnit = "", false
		steps = append(steps, old)
		retained = append(retained, old)
	}
	return steps, retained
}

// Main owns necessity and takeover meaning; the runtime owns durable state and
// the provenance of the quoted user instruction. Missing judgment cannot erase
// an obligation. This normalization is part of the existing Plan transaction.
func assessRunPlanCancellationsTx(ctx context.Context, tx *sql.Tx, tenant, run string, version int, steps []RunPlanStep) ([]RunPlanStep, error) {
	if version < assessedPlanCancellationContractVersion {
		return nil, nil
	}
	original, err := originalPlanCriteriaTx(ctx, tx, tenant, run)
	if err != nil {
		return nil, err
	}
	var deferred []RunPlanStep
	for i := range steps {
		s := &steps[i]
		s.CancellationDisposition = strings.TrimSpace(s.CancellationDisposition)
		s.CancellationReason = strings.TrimSpace(s.CancellationReason)
		s.UserTakeoverQuote = strings.TrimSpace(s.UserTakeoverQuote)
		s.ReplacementStepID = strings.TrimSpace(s.ReplacementStepID)
		s.ScopeChangeQuote = strings.TrimSpace(s.ScopeChangeQuote)
		if s.Status != "cancelled" {
			if s.Status != "pending" || s.CancellationDisposition != "unfinished" {
				s.CancellationDisposition, s.CancellationReason = "", ""
			}
			s.UserTakeoverQuote, s.ReplacementStepID, s.ScopeChangeQuote = "", "", ""
			continue
		}
		switch s.CancellationDisposition {
		case "", "unfinished":
			s.Status = "pending"
			s.CancellationDisposition = "unfinished"
			deferred = append(deferred, *s)
		case "not_required", "user_takeover":
			if !assessedCancellation(s.RunPlanStepInput) {
				return nil, &planCancellationPreconditionError{fmt.Sprintf("step %s cancellation requires a reason and user_takeover requires an exact user quote", s.StepID)}
			}
			if s.CancellationDisposition == "not_required" && version >= scopeEvidencePlanContractVersion {
				covered, err := cancellationScopeEvidenceTx(ctx, tx, tenant, run, *s, steps, original)
				if err != nil {
					return nil, err
				}
				if !covered {
					s.Status, s.CancellationDisposition = "pending", "unfinished"
					s.ReplacementStepID, s.ScopeChangeQuote = "", ""
					deferred = append(deferred, *s)
				}
			}
			if s.CancellationDisposition == "user_takeover" {
				found, err := hasUserTakeoverQuoteTx(ctx, tx, tenant, run, s.UserTakeoverQuote)
				if err != nil {
					return nil, err
				}
				if !found {
					return nil, &planCancellationPreconditionError{fmt.Sprintf("step %s takeover quote was not found in this work lineage's actual user input", s.StepID)}
				}
			}
		default:
			return nil, &planCancellationPreconditionError{fmt.Sprintf("step %s has an invalid cancellation_disposition", s.StepID)}
		}
	}
	return deferred, nil
}

// Main explains whether evidence covers the goal; this transaction checks the
// referenced state, unchanged criterion, and actual user-input provenance. No
// command name or wording of a failure decides necessity.
func cancellationScopeEvidenceTx(ctx context.Context, tx *sql.Tx, tenant, run string, step RunPlanStep, steps []RunPlanStep, original map[string]string) (bool, error) {
	if step.ScopeChangeQuote != "" {
		found, err := hasUserTakeoverQuoteTx(ctx, tx, tenant, run, step.ScopeChangeQuote)
		if err != nil {
			return false, err
		}
		if !found {
			return false, &planCancellationPreconditionError{fmt.Sprintf("step %s scope_change_quote was not found in this work lineage's actual user input", step.StepID)}
		}
		return true, nil
	}
	criterion, declared := original[step.StepID]
	if !declared {
		criterion = step.SuccessCriteria
	}
	if strings.TrimSpace(criterion) == "" || step.ReplacementStepID == "" || step.ReplacementStepID == step.StepID {
		return false, nil
	}
	for _, replacement := range steps {
		if replacement.StepID != step.ReplacementStepID || replacement.Status != "completed" {
			continue
		}
		return normalizeRunPlanText(criterion) == normalizeRunPlanText(replacement.SuccessCriteria) &&
			(!step.VerificationRequired || replacement.VerificationRequired), nil
	}
	return false, nil
}

func hasUserTakeoverQuoteTx(ctx context.Context, tx *sql.Tx, tenant, run, quote string) (bool, error) {
	// Traverse only exact continuation edges for the same person, never a
	// Thread label, another open task, tool prose, or an unconsumed mailbox row.
	rows, err := tx.QueryContext(ctx, `WITH RECURSIVE lineage(id,person_id,input_summary,depth) AS (
		SELECT id,person_id,input_summary,0 FROM runs WHERE tenant_id=? AND id=?
		UNION ALL SELECT p.id,p.person_id,p.input_summary,l.depth+1 FROM lineage l
		JOIN runs c ON c.id=l.id JOIN runs p ON p.id=c.resumes_run_id
		WHERE p.tenant_id=? AND p.person_id=l.person_id AND l.depth<32
	)
	SELECT l.input_summary FROM lineage l WHERE NOT EXISTS (
		SELECT 1 FROM task_events e WHERE e.run_id=l.id AND e.type='run.started'
		AND COALESCE(NULLIF(json_extract(e.payload_json,'$.origin'),''),'user')<>'user')
    UNION ALL SELECT json_extract(e.payload_json,'$.approval_intent.snapshot.raw_user_text')
    FROM task_events e JOIN lineage l ON l.id=e.run_id WHERE e.type='run.started'
      AND COALESCE(NULLIF(json_extract(e.payload_json,'$.origin'),''),'user')='user'
      AND COALESCE(json_extract(e.payload_json,'$.approval_intent.snapshot.source'),'') IN ('','direct','continuation')
      AND json_extract(e.payload_json,'$.approval_intent.snapshot.raw_user_text') IS NOT NULL
	UNION ALL SELECT m.content FROM steering_mailbox m JOIN lineage l ON l.id=m.run_id
	WHERE m.tenant_id=? AND m.person_id=l.person_id AND m.status='consumed'`, tenant, run, tenant, tenant)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			return false, err
		}
		if strings.Contains(content, quote) {
			return true, nil
		}
	}
	return false, rows.Err()
}
