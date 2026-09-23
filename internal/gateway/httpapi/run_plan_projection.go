package httpapi

import (
	"context"
	"fmt"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/tools"
	"selfmind/internal/verification"
)

// controlRunPlanProjection is the production adapter at the Run-plan seam. It
// hides durable plan versioning, work-unit projection, Skill candidate
// preparation, and completion validation from the generic task tools.
type controlRunPlanProjection struct {
	coordinator     *RunCoordinator
	identity        *control.IdentityContext
	run             *control.Run
	invocationScope kernel.ToolInvocationScope
}

func (c *RunCoordinator) newRunPlanProjection(identity *control.IdentityContext, run *control.Run, scope kernel.ToolInvocationScope) tools.RunPlanProjection {
	if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil || run == nil {
		return nil
	}
	return &controlRunPlanProjection{coordinator: c, identity: identity, run: run, invocationScope: scope}
}

func (p *controlRunPlanProjection) Project(ctx context.Context, state tools.PlanState) (tools.PlanProjectionResult, error) {
	if p == nil || p.coordinator == nil || p.coordinator.srv == nil || p.coordinator.srv.Control == nil || p.identity == nil || p.run == nil {
		return tools.PlanProjectionResult{}, fmt.Errorf("run plan projection is unavailable")
	}
	input := make([]control.RunPlanStepInput, 0, len(state.Plan))
	for _, step := range state.Plan {
		input = append(input, control.RunPlanStepInput{
			StepID: step.StepID, Step: step.Step, Status: step.Status,
			SuccessCriteria:        step.SuccessCriteria,
			VerificationRequired:   step.VerificationRequired,
			ReusePriorVerification: step.ReusePriorVerification,
			ReuseReason:            step.ReuseReason,
			WorkUnitID:             step.WorkUnitID, WorkUnit: step.WorkUnit,
		})
	}
	projection, err := p.coordinator.srv.Control.SyncRunPlan(ctx, p.identity.TenantID, p.run.ID, state.Explanation, input)
	if err != nil {
		return tools.PlanProjectionResult{}, err
	}
	review := []string{}
	// A step that arrives completed under a different acceptance bar than the
	// one that declared it is how a false completion stays invisible: the plan
	// still resolves, and the bar it resolved against is gone. Record the pair
	// so an audit can see the bar move; nothing here judges the change, because
	// restating a criterion can be honest replanning.
	for _, change := range projection.CriteriaRestated {
		if len(review) < 8 {
			review = append(review, fmt.Sprintf("Step %s changed acceptance from %q to %q. Judge this against the original user scope and explain any authorized scope change before finishing.", change.StepID, boundedReviewCriterion(change.From), boundedReviewCriterion(change.To)))
		}
		_, _ = p.coordinator.srv.Control.AppendEvent(ctx, control.Event{
			RunID:      p.run.ID,
			Type:       "plan.criteria_restated",
			Visibility: "task",
			Channel:    p.run.Channel,
			Payload: mustJSON(map[string]interface{}{
				"step_id": change.StepID, "step": change.Step,
				"from": change.From, "to": change.To,
			}),
		})
	}
	for _, step := range projection.VerificationDeferred {
		if len(review) >= 8 {
			break
		}
		if step.Status == "completed" && step.VerificationRequired {
			review = append(review, fmt.Sprintf("Step %s remains in_progress because its first durable snapshot marked it completed before required verification could be associated. Run verify now; the runtime will bind that check to this server-issued step id, then resubmit the complete snapshot.", step.StepID))
			continue
		}
		review = append(review, fmt.Sprintf("Step %s returned to pending while required verification for an earlier step remains open. Keep it pending until that check passes, then resubmit the complete snapshot.", step.StepID))
	}
	plan := tools.PlanState{Explanation: projection.Plan.Explanation, Plan: make([]tools.PlanStep, 0, len(projection.Plan.Steps))}
	for _, step := range projection.Plan.Steps {
		plan.Plan = append(plan.Plan, tools.PlanStep{
			StepID: step.StepID, Step: step.Step, Status: step.Status,
			SuccessCriteria:        step.SuccessCriteria,
			VerificationRequired:   step.VerificationRequired,
			ReusePriorVerification: step.ReusePriorVerification,
			ReuseReason:            step.ReuseReason,
			WorkUnitID:             step.WorkUnitID, WorkUnit: step.WorkUnit,
		})
	}
	workUnits := make([]tools.PlanWorkUnitIdentity, 0, len(projection.WorkUnits))
	for _, unit := range projection.WorkUnits {
		identity := tools.PlanWorkUnitIdentity{
			ID: unit.ID, Sequence: unit.Sequence, Goal: unit.GoalDigest,
			PlanStatus: unit.PlanStatus,
		}
		if unit.PlanStatus == "in_progress" {
			bindingBlocksCandidates := false
			if unit.RelatedTaskID != "" {
				binding, bindingErr := p.coordinator.srv.Control.GetTaskSkillBinding(ctx, p.identity.TenantID, p.identity.PersonID, unit.RelatedTaskID)
				if bindingErr == nil && binding != nil && binding.State != control.TaskSkillBindingReleased {
					bindingBlocksCandidates = true
					if binding.State == control.TaskSkillBindingActive {
						identity.BoundSkillName = binding.SkillName
					}
				}
			}
			if !bindingBlocksCandidates {
				candidateArgs := tools.WithSkillStorage(map[string]interface{}{
					"_tenant_id": p.identity.TenantID, "_context": ctx, "_invocation_scope": p.invocationScope,
				}, p.coordinator.srv.SkillStorage)
				_, catalog, _, prepared := p.coordinator.prepareSkillCandidateSnapshot(
					ctx, p.identity, p.run.ID, unit.ID, unit.GoalDigest, p.invocationScope, candidateArgs)
				if prepared {
					identity.SkillCatalog = catalog
				}
			}
		}
		workUnits = append(workUnits, identity)
	}
	return tools.PlanProjectionResult{AcceptanceReview: review, Plan: plan, Version: projection.Plan.Version, Changed: projection.Changed, WorkUnits: workUnits}, nil
}

func (p *controlRunPlanProjection) ValidateCompletion(ctx context.Context) error {
	if p == nil || p.coordinator == nil || p.coordinator.srv == nil || p.coordinator.srv.Control == nil || p.identity == nil || p.run == nil {
		return fmt.Errorf("run plan projection is unavailable")
	}
	if err := p.coordinator.srv.Control.ValidateRunCompletion(ctx, p.identity.TenantID, p.run.ID); err != nil {
		return err
	}
	verdict, files := p.coordinator.evidenceOutcome(ctx, p.identity.TenantID, p.run.ID)
	if verificationRequiresResume(verdict, files) {
		return fmt.Errorf("completion requires verification: %s %s", verdict.Summary, verificationNextStep(verdict))
	}
	return nil
}

func (p *controlRunPlanProjection) ValidateVerification(ctx context.Context, binding verification.Binding, cwd string) error {
	return p.coordinator.srv.Control.ValidateVerificationReplacement(ctx, p.identity.TenantID, p.run.ID, binding, cwd)
}

func boundedReviewCriterion(value string) string {
	runes := []rune(value)
	if len(runes) > 500 {
		return string(runes[:500]) + "…"
	}
	return value
}

func (p *controlRunPlanProjection) ResolveVerification(ctx context.Context, binding verification.Binding, cwd string) (*verification.Binding, error) {
	return p.coordinator.srv.Control.ResolveVerificationBinding(ctx, p.identity.TenantID, p.run.ID, binding, cwd)
}
