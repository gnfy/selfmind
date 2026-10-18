package httpapi

import (
	"context"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/verification"
)

// Current execution state does not require a historical parent or full Thread
// history. Read Plan and facts together, never promote a parent's observations
// to current evidence just because the new Run continues it.
func (c *RunCoordinator) selectCurrentRunPlan(ctx context.Context, task *control.Task, run *control.Run, showEmpty bool, selected *kernel.TaskRuntimeContext) {
	if run == nil || run.TaskID != task.ID || run.PersonID != task.PersonID {
		return
	}
	snapshot, err := c.srv.Control.ReadRunPlanSnapshot(ctx, task.TenantID, task.PersonID, run.ID)
	if err != nil {
		selected.ClosureFacts = &verification.ClosureFacts{Available: false}
		return
	}
	if plan := snapshot.Plan; plan != nil {
		selected.PlanVersion = plan.Version
		for _, step := range plan.Steps {
			selected.Plan = append(selected.Plan, kernel.PlanItem{
				StepID: step.StepID, Step: step.Step, Status: step.Status,
				SuccessCriteria: step.SuccessCriteria, VerificationRequired: step.VerificationRequired,
				CancellationDisposition: step.CancellationDisposition, CancellationReason: step.CancellationReason, UserTakeoverQuote: step.UserTakeoverQuote,
				ReplacementStepID: step.ReplacementStepID, ScopeChangeQuote: step.ScopeChangeQuote,
			})
		}
	}
	if showEmpty || snapshot.Plan != nil || !snapshot.ClosureFacts.Available || len(snapshot.ClosureFacts.Evidence) > 0 {
		selected.ClosureFacts = &snapshot.ClosureFacts
	}
}
