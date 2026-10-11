package tools

import (
	"context"
	"fmt"

	"selfmind/internal/control"
)

func (t *WorkInspectTool) inspectPlanPage(ctx context.Context, tenant string, run *control.Run, offset int, result map[string]interface{}) error {
	if offset < 0 {
		return fmt.Errorf("plan_offset must be nonnegative")
	}
	plan, err := t.store.LatestRunPlan(ctx, tenant, run.ID)
	if err != nil {
		return err
	}
	if plan == nil {
		return nil
	}
	if offset > len(plan.Steps) {
		return fmt.Errorf("plan_offset exceeds the current Plan's %d steps", len(plan.Steps))
	}
	copy := *plan
	copy.Explanation = workBound(copy.Explanation, 400)
	end := min(offset+12, len(plan.Steps))
	copy.Steps = append([]control.RunPlanStep(nil), plan.Steps[offset:end]...)
	selected := make(map[string]bool, len(copy.Steps))
	for i := range copy.Steps {
		selected[copy.Steps[i].StepID] = true
		copy.Steps[i].Step = workBound(copy.Steps[i].Step, 300)
		copy.Steps[i].SuccessCriteria = workBound(copy.Steps[i].SuccessCriteria, 300)
	}
	result["plan"] = struct {
		control.RunPlan
		Steps []DisplayedPlanStep `json:"steps"`
	}{copy, DisplayedPlanSteps(ctx, t.store, tenant, run.ID, copy.Steps)}
	page := map[string]int{"offset": offset, "total_steps": len(plan.Steps)}
	if end < len(plan.Steps) {
		page["next_offset"] = end
	}
	result["plan_page"] = page
	if run.ResumesRunID == "" {
		return nil
	}
	prior, err := t.store.ListInheritedPlanEvidence(ctx, tenant, run.ID)
	if err != nil {
		return err
	}
	var refs []control.InheritedPlanEvidence
	for _, item := range prior {
		if !selected[item.StepID] {
			continue
		}
		item.SourceCriterion = workBound(item.SourceCriterion, 300)
		item.ObservedCriterion = workBound(item.ObservedCriterion, 300)
		item.Target = workBound(item.Target, 300)
		item.ReuseBlockedReason = workBound(item.ReuseBlockedReason, 300)
		refs = append(refs, item)
	}
	result["source_step_evidence"] = refs
	return nil
}
