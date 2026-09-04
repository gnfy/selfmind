package httpapi

import (
	"context"
	"fmt"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/tools"
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
			SuccessCriteria:      step.SuccessCriteria,
			VerificationRequired: step.VerificationRequired,
			WorkUnitID:           step.WorkUnitID, WorkUnit: step.WorkUnit,
		})
	}
	projection, err := p.coordinator.srv.Control.SyncRunPlan(ctx, p.identity.TenantID, p.run.ID, state.Explanation, input)
	if err != nil {
		return tools.PlanProjectionResult{}, err
	}
	plan := tools.PlanState{Explanation: projection.Plan.Explanation, Plan: make([]tools.PlanStep, 0, len(projection.Plan.Steps))}
	for _, step := range projection.Plan.Steps {
		plan.Plan = append(plan.Plan, tools.PlanStep{
			StepID: step.StepID, Step: step.Step, Status: step.Status,
			SuccessCriteria:      step.SuccessCriteria,
			VerificationRequired: step.VerificationRequired,
			WorkUnitID:           step.WorkUnitID, WorkUnit: step.WorkUnit,
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
	return tools.PlanProjectionResult{Plan: plan, Version: projection.Plan.Version, Changed: projection.Changed, WorkUnits: workUnits}, nil
}

func (p *controlRunPlanProjection) ValidateCompletion(ctx context.Context) error {
	if p == nil || p.coordinator == nil || p.coordinator.srv == nil || p.coordinator.srv.Control == nil || p.identity == nil || p.run == nil {
		return fmt.Errorf("run plan projection is unavailable")
	}
	return p.coordinator.srv.Control.ValidateRunCompletion(ctx, p.identity.TenantID, p.run.ID)
}
