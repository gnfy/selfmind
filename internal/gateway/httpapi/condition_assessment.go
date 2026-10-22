package httpapi

import (
	"context"
	"fmt"

	"selfmind/internal/gateway/api"
	"selfmind/internal/verification"
)

func applyConditionProjection(out *api.VerificationOutcome, condition verification.AssessmentProjection, err error) {
	out.ConditionState = condition.State
	out.Conditions = condition.Conditions
	out.ConditionBlockers = condition.Blockers
	out.ConditionReviewComplete = condition.ReviewComplete
	if err != nil {
		out.ConditionState = "unknown"
		out.ConditionReviewComplete = false
		out.ConditionBlockers = []string{"The condition assessment could not be revalidated against current evidence; review its references and inputs."}
	}
}

func (p *controlRunPlanProjection) PrepareConditionAssessments(ctx context.Context, status string, input []verification.Assessment) ([]verification.Assessment, error) {
	current := input
	condition, err := p.coordinator.srv.Control.ResolveConditionAssessments(ctx, p.identity.TenantID, p.run.ID, current)
	for err != nil && status != "done" && len(current) > 0 {
		next, dropped := dropUngroundedStepMetAssessment(current, err)
		if !dropped {
			break
		}
		current = next
		if len(current) == 0 {
			return nil, nil
		}
		condition, err = p.coordinator.srv.Control.ResolveConditionAssessments(ctx, p.identity.TenantID, p.run.ID, current)
	}
	if err != nil {
		return nil, err
	}
	if status == "done" {
		if err := p.coordinator.srv.Control.ValidateRunCompletionWithAssessments(ctx, p.identity.TenantID, p.run.ID, current); err != nil {
			return nil, err
		}
		verdict, files := p.coordinator.evidenceOutcome(ctx, p.identity.TenantID, p.run.ID)
		if verdict != nil {
			applyConditionProjection(verdict, condition, nil)
		}
		if verificationRequiresResume(verdict, files) {
			return nil, fmt.Errorf("completion requires evidence review: %s %s", verdict.Summary, verificationNextStep(verdict))
		}
	}
	return condition.Conditions, nil
}

func dropUngroundedStepMetAssessment(input []verification.Assessment, err error) ([]verification.Assessment, bool) {
	if err == nil {
		return input, false
	}
	msg := err.Error()
	out := make([]verification.Assessment, 0, len(input))
	dropped := false
	for _, a := range input {
		if !dropped && a.StepID != "" && len(a.EvidenceRefs) == 0 && a.Result == "met" &&
			(msg == fmt.Sprintf("step %q has no effective verify check in this Run; verify that step first", a.StepID) ||
				msg == fmt.Sprintf("step %q has no effective current or explicitly adopted check; verify that step or explicitly adopt still-valid prior verification first", a.StepID)) {
			dropped = true
			continue
		}
		out = append(out, a)
	}
	return out, dropped
}
