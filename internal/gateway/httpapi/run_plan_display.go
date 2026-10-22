package httpapi

import (
	"context"
	"encoding/json"

	"selfmind/internal/control"
	"selfmind/internal/tools"
)

// Presence, including an empty accepted plan, is authoritative. A failed read
// or ownership mismatch must not fall back to older state from the event tail.
func (d *Server) durableRunPlanPayload(ctx context.Context, identity *control.IdentityContext, taskID, runID string) (string, bool) {
	run, err := d.Control.GetRun(ctx, identity.TenantID, runID)
	if err != nil || run == nil || run.PersonID != identity.PersonID || run.TaskID != taskID {
		return "", true
	}
	plan, err := d.Control.LatestRunPlan(ctx, identity.TenantID, runID)
	if err != nil {
		return "", true
	}
	if plan == nil {
		return "", false
	}
	payload, err := json.Marshal(struct {
		Explanation string                    `json:"explanation,omitempty"`
		Plan        []tools.DisplayedPlanStep `json:"plan"`
		Version     int                       `json:"plan_version"`
	}{plan.Explanation, tools.DisplayedPlanSteps(ctx, d.Control, identity.TenantID, runID, plan.Steps), plan.Version})
	if err != nil {
		return "", true
	}
	return string(payload), true
}
