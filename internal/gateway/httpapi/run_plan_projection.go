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
			CancellationDisposition: step.CancellationDisposition, CancellationReason: step.CancellationReason, UserTakeoverQuote: step.UserTakeoverQuote,
			ReplacementStepID: step.ReplacementStepID, ScopeChangeQuote: step.ScopeChangeQuote,
			SuccessCriteria:        step.SuccessCriteria,
			VerificationRequired:   step.VerificationRequired,
			ReusePriorVerification: step.ReusePriorVerification,
			ReuseReason:            step.ReuseReason,
			WorkUnitID:             step.WorkUnitID, WorkUnit: step.WorkUnit,
		})
	}
	var projection control.RunPlanProjection
	var err error
	if state.Mode == "patch" {
		version, _ := kernel.CurrentRunExecutionPlan(ctx)
		projection, err = p.coordinator.srv.Control.PatchRunPlan(ctx, p.identity.TenantID, p.run.ID, version, state.Explanation, input)
	} else {
		projection, err = p.coordinator.srv.Control.SyncRunPlan(ctx, p.identity.TenantID, p.run.ID, state.Explanation, input)
	}
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
	for _, deferral := range projection.CancellationDeferred {
		if len(review) >= 8 {
			break
		}
		review = append(review, cancellationDeferralReview(deferral))
	}
	for _, step := range projection.VerificationDeferred {
		if len(review) >= 8 {
			break
		}
		if step.SourceStepID != "" {
			status := step.Status
			for _, current := range projection.Plan.Steps {
				if current.StepID == step.StepID {
					status = current.Status
					break
				}
			}
			reason := step.VerificationDeferralReason()
			if reason == "" {
				reason = "prior verification from an earlier Run no longer validates after changes in this Run"
			}
			review = append(review, fmt.Sprintf("Step %s was reopened (%s) because %s. Run verify with step_id=%s and mark it completed with the fresh evidence.", step.StepID, status, reason, step.StepID))
			continue
		}
		if step.Status == "completed" && step.VerificationRequired {
			status := "pending"
			for _, accepted := range projection.Plan.Steps {
				if accepted.StepID == step.StepID {
					status = accepted.Status
					break
				}
			}
			review = append(review, fmt.Sprintf("Step %s remains %s because required verification is missing. Use verify with step_id=%s, or explicitly associate a supported executed observation at finish; submit only changed steps.", step.StepID, status, step.StepID))
			continue
		}
		review = append(review, fmt.Sprintf("Step %s returned to pending while required verification for an earlier step remains open. Keep it pending until that check passes, then resubmit the complete snapshot.", step.StepID))
	}
	plan := tools.PlanState{Explanation: projection.Plan.Explanation, Plan: make([]tools.PlanStep, 0, len(projection.Plan.Steps))}
	origins := tools.PlanQuoteOrigins(ctx, p.coordinator.srv.Control, p.identity.TenantID, p.run.ID, projection.Plan.Steps)
	for _, step := range projection.Plan.Steps {
		plan.Plan = append(plan.Plan, tools.PlanStep{
			StepID: step.StepID, Step: step.Step, Status: step.Status,
			CancellationDisposition: step.CancellationDisposition, CancellationReason: step.CancellationReason, UserTakeoverQuote: step.UserTakeoverQuote,
			ReplacementStepID: step.ReplacementStepID, ScopeChangeQuote: step.ScopeChangeQuote, CancellationAuthorizedBy: origins[step.CancellationQuoteSource],
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
	// The Plan is already committed. A failed auxiliary read must not turn that
	// accepted mutation into a refusal or advertise an empty successful review.
	facts, factsErr := p.coordinator.srv.Control.RunClosureFacts(ctx, p.identity.TenantID, p.run.ID)
	if factsErr != nil || facts.PlanVersion != projection.Plan.Version {
		facts = verification.ClosureFacts{Available: false}
	}
	return tools.PlanProjectionResult{ClosureFacts: &facts, AcceptanceReview: review, Plan: plan, Version: projection.Plan.Version, Changed: projection.Changed, WorkUnits: workUnits}, nil
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

func (p *controlRunPlanProjection) GuardrailRevision(ctx context.Context, completedStepIDs []string) (string, error) {
	if p == nil || p.coordinator == nil || p.coordinator.srv == nil || p.coordinator.srv.Control == nil || p.identity == nil || p.run == nil {
		return "", fmt.Errorf("run plan projection is unavailable")
	}
	return p.coordinator.srv.Control.RunPlanEvidenceRevision(ctx, p.identity.TenantID, p.run.ID, completedStepIDs)
}

func (p *controlRunPlanProjection) ValidateVerification(ctx context.Context, binding verification.Binding, cwd string) error {
	return p.coordinator.srv.Control.ValidateVerificationReplacement(ctx, p.identity.TenantID, p.run.ID, binding, cwd)
}

// cancellationDeferralReview names the one fact the runtime could not find for
// a cancellation it kept open, and an exact way to settle it. The settling
// bar follows the step: a step planned without success_criteria is never told
// to repeat a criterion it does not have.
func cancellationDeferralReview(d control.RunPlanCancellationDeferral) string {
	step, replacement := d.Step.StepID, d.ReplacementStepID
	var missing string
	switch d.Reason {
	case control.CancellationDeferredUnfinished:
		return fmt.Sprintf("Step %s remains pending as unfinished necessary work. Continue independent work, and finish blocked or waiting if it cannot proceed.", step)
	case control.CancellationDeferredOmitted:
		return fmt.Sprintf("Step %s remains pending: the complete snapshot omitted it. Include it with its current status; cancelling it needs an assessed cancellation_disposition.", step)
	case control.CancellationDeferredNoDisposition:
		missing = "the cancellation has no cancellation_disposition"
	case control.CancellationDeferredNoCoverage:
		missing = "not_required cites neither a replacement_step_id nor a scope_change_quote"
	case control.CancellationDeferredReplacementUnknown:
		missing = fmt.Sprintf("replacement_step_id %q is not another step of this plan", replacement)
	case control.CancellationDeferredReplacementOpen:
		missing = fmt.Sprintf("replacement step %s is %s, not completed", replacement, d.Replacement.Status)
	case control.CancellationDeferredCriterionChanged:
		missing = fmt.Sprintf("replacement step %s has success_criteria %q instead of this step's %q", replacement, boundedReviewCriterion(d.Replacement.SuccessCriteria), boundedReviewCriterion(d.Criterion))
	case control.CancellationDeferredCriterionUnstated:
		missing = fmt.Sprintf("replacement step %s states no success_criteria", replacement)
	case control.CancellationDeferredVerificationDropped:
		missing = fmt.Sprintf("replacement step %s does not require the verification this step requires", replacement)
	default:
		missing = "its cancellation is not assessed"
	}
	cite := "a completed replacement_step_id that states its own success_criteria (this step has none to repeat)"
	if d.Criterion != "" {
		cite = fmt.Sprintf("a completed replacement_step_id whose success_criteria is exactly %q", boundedReviewCriterion(d.Criterion))
		if d.Step.VerificationRequired {
			cite += " and that requires verification"
		}
	}
	return fmt.Sprintf("Step %s remains pending: %s. To settle it as not_required, explain the coverage in cancellation_reason and cite %s, or a scope_change_quote copied exactly from one user instruction. An explicit user takeover uses user_takeover with user_takeover_quote. Otherwise keep it open, continue independent work, and finish blocked or waiting if it cannot proceed.", step, missing, cite)
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
