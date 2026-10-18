package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// earlierRunEvidenceProbeReason stands in for Main's reason while the runtime
// only asks whether an adoption would validate. It is never persisted.
const earlierRunEvidenceProbeReason = "Eligibility probe only; Main must assess the prior observation."

// earlierRunEvidenceNoteTx says what this Run knows about an evidence id that is
// not one of its own checks. Call ids restart in every Run, so an id read in an
// earlier Run's tool record never selects anything here. What the Run can offer
// is the Plan step that stands on that observation and whether the runtime would
// adopt it, so a refusal tells the model the way out instead of leaving it to
// reconstruct the evidence boundary. Empty when the id is not an imported step's
// observation, or when the lookup itself fails: the refusal stands without it.
func earlierRunEvidenceNoteTx(ctx context.Context, tx *sql.Tx, tenant, runID, ref string) string {
	if strings.TrimSpace(ref) == "" {
		return ""
	}
	items, err := inheritedPlanEvidenceTx(ctx, tx, tenant, runID)
	if err != nil {
		return ""
	}
	var stepID string
	for _, item := range items {
		if item.CheckID == ref {
			stepID = item.StepID
			break
		}
	}
	if stepID == "" {
		return ""
	}
	plan, err := latestRunPlanTx(ctx, tx, tenant, runID)
	if err != nil || plan == nil {
		return ""
	}
	for _, step := range plan.Steps {
		if step.StepID != stepID {
			continue
		}
		adopted := step.ReusePriorVerification
		candidate := step
		candidate.ReusePriorVerification = true
		if strings.TrimSpace(candidate.ReuseReason) == "" {
			candidate.ReuseReason = earlierRunEvidenceProbeReason
		}
		err := validatePriorVerificationReuseTx(ctx, tx, tenant, runID, candidate)
		var precondition *completionPreconditionError
		if err != nil && !errors.As(err, &precondition) {
			return ""
		}
		switch {
		case adopted && err == nil:
			return fmt.Sprintf("%s is the earlier-Run observation that step %s already stands on; an adopted observation counts through its step and needs no assessment of its own. For a new condition judgment about that step, select step_id=%s instead of citing this historical id.", ref, stepID, stepID)
		case adopted:
			return fmt.Sprintf("%s is the earlier-Run observation that step %s stands on, and its adoption no longer validates (%s). Run verify with step_id=%s and cite the new check.", ref, stepID, precondition.message, stepID)
		case err == nil:
			return fmt.Sprintf("%s is the earlier-Run observation behind step %s, not a check made in this Run. If it still holds, complete that step with update_plan reuse_prior_verification=true and a reuse_reason; otherwise run verify with step_id=%s and cite the new check.", ref, stepID, stepID)
		default:
			return fmt.Sprintf("%s is the earlier-Run observation behind step %s, not a check made in this Run, and the runtime cannot adopt it now (%s). Run verify with step_id=%s and cite the new check.", ref, stepID, precondition.message, stepID)
		}
	}
	return ""
}

func (s *Store) earlierRunEvidenceNote(ctx context.Context, tenant, runID, ref string) string {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ""
	}
	defer tx.Rollback()
	return earlierRunEvidenceNoteTx(ctx, tx, normalizeTenant(tenant), runID, ref)
}
