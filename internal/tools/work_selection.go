package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"selfmind/internal/control"
)

// WorkSelectTool records Main's typed interpretation of one exact historical
// Run and, for a same-domain resume, claims that Run's continuation at once so
// the current Main turn continues the work in place. The claim is a control
// transaction that revalidates person, execution domain, checkpoint state, the
// effect boundary, and parent ownership; a mismatch leaves a typed proposal
// that the gateway commits as a transfer child after the turn.
type WorkSelectTool struct {
	store *control.Store
}

func NewWorkSelectTool(store *control.Store) *WorkSelectTool {
	return &WorkSelectTool{store: store}
}

func (t *WorkSelectTool) Name() string { return "work_select" }

func (t *WorkSelectTool) Description() string {
	return "Propose how the current request relates to one exact inspected historical run. Use observe for a read-only status/result question, or resume to continue that work. A same-domain resume is claimed immediately and returns the run's resume context so you continue the work in this turn; otherwise the gateway queues an exact continuation after this turn."
}

func (t *WorkSelectTool) Schema() ToolSchema {
	return ToolSchema{
		Type: "object",
		Properties: map[string]PropertyDef{
			"action": {Type: "string", Enum: []string{"observe", "resume"}, Description: "observe reads/reports prior state; resume continues that work, directly in this turn when the execution domain matches."},
			"run_id": {Type: "string", Description: "Exact run_id already supported by work_search/work_inspect evidence."},
		},
		Required: []string{"action", "run_id"},
	}
}

func (t *WorkSelectTool) Metadata() ToolMetadata {
	return ToolMetadata{Exposure: ToolExposureDirect, RiskLevel: ToolRiskLow, Category: "task"}
}

func (t *WorkSelectTool) Execute(args map[string]interface{}) (string, error) {
	if t == nil || t.store == nil {
		return "", fmt.Errorf("work selection is unavailable")
	}
	scope, ok := InvocationScopeFromArgs(args)
	if !ok || strings.TrimSpace(scope.ControlTenantID) == "" || strings.TrimSpace(scope.PersonID) == "" ||
		strings.TrimSpace(scope.TaskID) == "" || strings.TrimSpace(scope.RunID) == "" {
		return "", fmt.Errorf("authenticated run scope is required")
	}
	action := strings.ToLower(strings.TrimSpace(taskStringArg(args, "action")))
	if action != "observe" && action != "resume" {
		return "", fmt.Errorf("action must be observe or resume")
	}
	targetRunID := strings.TrimSpace(taskStringArg(args, "run_id"))
	if targetRunID == "" || targetRunID == scope.RunID {
		return "", fmt.Errorf("an exact historical run_id is required")
	}
	ctx := ContextFromArgs(args)
	current, err := t.store.GetRun(ctx, scope.ControlTenantID, scope.RunID)
	if err != nil {
		return "", err
	}
	if current == nil || current.PersonID != scope.PersonID || current.Status != "running" {
		return "", fmt.Errorf("current run is not eligible for work selection")
	}
	// A direct claim earlier in this turn already moved the run onto the
	// selected work: repeating that selection converges, and a different
	// resume target is the one auditable pre-effect correction.
	claimedThisTurn := current.ParentRunID != "" && current.TaskID != scope.TaskID
	if claimedThisTurn && current.ParentRunID == targetRunID {
		return t.directContinuationResult(ctx, scope.ControlTenantID, scope.PersonID, current, targetRunID)
	}
	if !claimedThisTurn && current.TaskID != scope.TaskID {
		return "", fmt.Errorf("current run is not eligible for work selection")
	}
	// After a claim the run's audit trail lives on the thread it continues.
	auditThreadID := current.TaskID
	target, err := t.store.GetRun(ctx, scope.ControlTenantID, targetRunID)
	if err != nil {
		return "", err
	}
	if target == nil || target.PersonID != scope.PersonID {
		return "", fmt.Errorf("target run is unavailable for the current person")
	}
	if action == "resume" {
		candidates, err := t.store.ListUnresolvedRuns(ctx, scope.ControlTenantID, scope.PersonID, target.TaskID, 20)
		if err != nil {
			return "", err
		}
		resumable := false
		for _, candidate := range candidates {
			if candidate.ID == target.ID {
				resumable = true
				break
			}
		}
		if !resumable {
			return "", fmt.Errorf("target run is no longer resumable")
		}
	}
	events, err := t.store.ListRunEvents(ctx, scope.ControlTenantID, scope.PersonID, auditThreadID, scope.RunID, 50)
	if err != nil {
		return "", err
	}
	var previousAction, previousRunID string
	repeated := false
	for _, event := range events {
		if event.Type != "work.selection" {
			continue
		}
		var existing struct {
			Action string `json:"action"`
			RunID  string `json:"run_id"`
		}
		if json.Unmarshal(event.Payload, &existing) != nil {
			return "", fmt.Errorf("the existing work selection audit is invalid")
		}
		previousAction = strings.ToLower(strings.TrimSpace(existing.Action))
		previousRunID = strings.TrimSpace(existing.RunID)
		repeated = previousAction == action && previousRunID == targetRunID
		break
	}
	if previousRunID != "" && !repeated {
		blocked, reason, err := t.store.RunSelectionEffectBoundary(ctx, scope.ControlTenantID, scope.PersonID, scope.RunID)
		if err != nil {
			return "", err
		}
		if blocked {
			return "", fmt.Errorf("the prior selection cannot be corrected after a material effect (%s); stop expanding it and ask the user how to proceed", reason)
		}
	}
	status := "proposed"
	if !repeated {
		payload := map[string]interface{}{
			"action": action, "run_id": targetRunID, "task_id": target.TaskID,
			"target_workspace_id": target.WorkspaceID,
		}
		if previousRunID != "" {
			status = "corrected"
			payload["correction_of"] = map[string]string{"action": previousAction, "run_id": previousRunID}
		}
		if _, err := t.store.AppendEvent(ctx, control.Event{
			TaskID: auditThreadID, RunID: scope.RunID, Type: "work.selection", Visibility: "task",
			Payload: mustToolJSON(payload),
		}); err != nil {
			return "", err
		}
	}
	if action != "resume" {
		return workSelectionResult(status, action, targetRunID, "The gateway will revalidate and commit this observation after the current Main turn. Do not perform work for the target run in the current execution scope."), nil
	}
	if claimedThisTurn {
		return t.retargetDirectContinuation(ctx, scope.ControlTenantID, scope.PersonID, current, target)
	}
	return t.tryDirectContinuation(ctx, scope.ControlTenantID, scope.PersonID, scope.RunID, target, status)
}

// retargetDirectContinuation is the pre-effect correction after a direct
// claim: the effect boundary was checked above, so the run may move to the
// corrected same-domain parent. A different domain cannot be corrected in
// place; the model must report and ask instead of guessing.
func (t *WorkSelectTool) retargetDirectContinuation(ctx context.Context, tenantID, personID string, current, target *control.Run) (string, error) {
	blocked, reason, err := t.store.RunSelectionEffectBoundary(ctx, tenantID, personID, current.ID)
	if err != nil {
		return "", err
	}
	if blocked {
		return "", fmt.Errorf("this turn already continues run %s and has produced effects (%s); the selection cannot be corrected now, so report the situation and ask the user how to proceed", current.ParentRunID, reason)
	}
	retargeted, err := t.store.RetargetInteractionContinuation(ctx, tenantID, personID, current.ID, target.ID)
	switch {
	case err == nil:
		return t.directContinuationResult(ctx, tenantID, personID, retargeted, target.ID)
	case errors.Is(err, control.ErrContinuationDomainMismatch), errors.Is(err, control.ErrParentCheckpointRequired):
		return "", fmt.Errorf("this turn already continues run %s in its execution scope; the corrected run lives in a different scope or needs checkpoint restoration and cannot be continued here. Report the correction and ask the user how to proceed", current.ParentRunID)
	default:
		return "", err
	}
}

// tryDirectContinuation claims the selected run for this turn when the
// interaction is still effect-free and shares the target's execution domain.
// Any typed mismatch keeps the proposal for the gateway's transfer commit.
func (t *WorkSelectTool) tryDirectContinuation(ctx context.Context, tenantID, personID, runID string, target *control.Run, status string) (string, error) {
	blocked, reason, err := t.store.RunSelectionEffectBoundary(ctx, tenantID, personID, runID)
	if err != nil {
		return "", err
	}
	if blocked {
		return workSelectionResult(status, "resume", target.ID, "This interaction already produced effects ("+reason+"), so the historical run cannot be continued in place. The gateway decides after this turn; report what happened and stop expanding the work."), nil
	}
	claimed, err := t.store.ClaimInteractionContinuation(ctx, tenantID, personID, runID, target.ID)
	switch {
	case err == nil:
		return t.directContinuationResult(ctx, tenantID, personID, claimed, target.ID)
	case errors.Is(err, control.ErrContinuationDomainMismatch), errors.Is(err, control.ErrParentCheckpointRequired):
		return workSelectionResult(status, "resume", target.ID, "The selected run lives in a different execution scope or needs its checkpoint restored, so the gateway will queue it as an exact continuation after this turn. Acknowledge briefly and finish this turn; do not perform the target's work here."), nil
	default:
		return "", err
	}
}

// directContinuationResult records the committed claim once and returns the
// parent's bounded resume context so the model continues in this turn.
func (t *WorkSelectTool) directContinuationResult(ctx context.Context, tenantID, personID string, claimed *control.Run, targetRunID string) (string, error) {
	if claimed == nil {
		return "", fmt.Errorf("claimed continuation run is unavailable")
	}
	committed := map[string]interface{}{
		"action": "resume", "run_id": targetRunID, "thread_id": claimed.TaskID, "commit_mode": "direct",
	}
	if _, err := t.store.AppendEvent(ctx, control.Event{
		TaskID: claimed.TaskID, RunID: claimed.ID, Type: "work.selection_committed", Visibility: "task",
		Channel: claimed.Channel, Payload: mustToolJSON(committed),
		IdempotencyKey: "work.selection_committed:" + claimed.ID + ":" + targetRunID,
	}); err != nil {
		return "", err
	}
	plan, planErr := t.inheritedPlanSteps(ctx, tenantID, personID, claimed.TaskID, targetRunID)
	if planErr != nil {
		return "", planErr
	}
	if len(plan) > 0 {
		if _, err := t.store.AppendEvent(ctx, control.Event{
			TaskID: claimed.TaskID, RunID: claimed.ID, Type: "plan.updated", Visibility: "task",
			Channel: claimed.Channel, Payload: mustToolJSON(map[string]interface{}{
				"plan": plan, "explanation": "Plan inherited from the continued run",
				"source": "parent_run", "parent_run_id": targetRunID,
			}),
			IdempotencyKey: "plan.inherited:" + claimed.ID + ":" + targetRunID,
		}); err != nil {
			return "", err
		}
	}
	resume, err := t.resumeContext(ctx, tenantID, personID, claimed.TaskID, targetRunID, plan)
	if err != nil {
		return "", err
	}
	encoded, _ := json.Marshal(map[string]interface{}{
		"status": "committed", "commit_mode": "direct", "action": "resume", "run_id": targetRunID,
		"thread_id":      claimed.TaskID,
		"resume_context": resume,
		"message":        "This turn now continues the selected run in the same execution scope. Use the resume context, do the remaining work here, keep the completed steps when you call update_plan, and finish with the real result rather than an acknowledgement.",
	})
	return string(encoded), nil
}

type planStepView struct {
	Step   string `json:"step"`
	Status string `json:"status"`
}

// inheritedPlanSteps prefers the durable run plan; runs recorded before the
// durable plan contract only carry plan.updated snapshots in their events.
func (t *WorkSelectTool) inheritedPlanSteps(ctx context.Context, tenantID, personID, threadID, parentRunID string) ([]planStepView, error) {
	if plan, err := t.store.LatestRunPlan(ctx, tenantID, parentRunID); err != nil {
		return nil, err
	} else if plan != nil && len(plan.Steps) > 0 {
		steps := make([]planStepView, 0, len(plan.Steps))
		for _, step := range plan.Steps {
			steps = append(steps, planStepView{Step: step.Step, Status: step.Status})
		}
		return steps, nil
	}
	events, err := t.store.ListRunEvents(ctx, tenantID, personID, threadID, parentRunID, 50)
	if err != nil {
		return nil, err
	}
	for _, event := range events {
		if event.Type != "plan.updated" {
			continue
		}
		var payload struct {
			Plan []planStepView `json:"plan"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && len(payload.Plan) > 0 {
			return payload.Plan, nil
		}
	}
	return nil, nil
}

func (t *WorkSelectTool) resumeContext(ctx context.Context, tenantID, personID, threadID, parentRunID string, plan []planStepView) (string, error) {
	handoff, err := t.store.RunHandoff(ctx, tenantID, personID, parentRunID)
	if err != nil {
		return "", err
	}
	parent, err := t.store.GetRun(ctx, tenantID, parentRunID)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	sb.WriteString("[SelfMind resume context]\n")
	sb.WriteString("You are continuing run " + parentRunID + " in its own execution scope; this turn is now that work.\n")
	if parent != nil {
		if input := strings.TrimSpace(parent.InputSummary); input != "" {
			sb.WriteString("Original request: " + workBound(input, 600) + "\n")
		}
		if parent.Status != "" {
			sb.WriteString("It stopped as " + parent.Status + ".\n")
		}
	}
	if handoff != nil {
		if summary := strings.TrimSpace(handoff.Summary); summary != "" {
			sb.WriteString("Summary: " + workBound(summary, 800) + "\n")
		}
		writeResumeList(&sb, "Done", boundedWorkStrings(handoff.DoneItems, 8, 240))
		writeResumeList(&sb, "Next steps", boundedWorkStrings(handoff.NextSteps, 8, 240))
		writeResumeList(&sb, "Changed files", boundedWorkStrings(handoff.ChangedFiles, 12, 240))
		writeResumeList(&sb, "Risks", boundedWorkStrings(handoff.Risks, 6, 240))
	}
	if len(plan) > 0 {
		sb.WriteString("Current plan:\n")
		for _, step := range plan {
			marker := "[ ]"
			switch strings.ToLower(strings.TrimSpace(step.Status)) {
			case "completed", "done":
				marker = "[x]"
			case "in_progress":
				marker = "[>]"
			}
			sb.WriteString("- " + marker + " " + workBound(step.Step, 240) + "\n")
		}
		sb.WriteString("This plan is inherited from the continued run. For multi-step work, call update_plan with a complete snapshot that keeps the completed steps, then continue from the in-progress step.\n")
	}
	sb.WriteString("Continue from this state now. Do not restart completed work unless the user asks for a restart.\n")
	sb.WriteString("[/SelfMind resume context]")
	return sb.String(), nil
}

func writeResumeList(sb *strings.Builder, label string, values []string) {
	if len(values) == 0 {
		return
	}
	sb.WriteString(label + ":\n")
	for _, value := range values {
		sb.WriteString("- " + value + "\n")
	}
}

func workSelectionResult(status, action, runID, message string) string {
	encoded, _ := json.Marshal(map[string]interface{}{
		"status": status, "action": action, "run_id": runID,
		"message": message,
	})
	return string(encoded)
}

func mustToolJSON(value interface{}) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}
