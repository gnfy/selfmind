package tools

import (
	"encoding/json"
	"fmt"
	"strings"

	"selfmind/internal/control"
)

// WorkSelectTool records Main's typed, advisory interpretation of one exact
// historical Run. The gateway commits it after the foreground turn, when it
// can revalidate execution scope, effect boundaries, queueing, and delivery.
type WorkSelectTool struct {
	store *control.Store
}

func NewWorkSelectTool(store *control.Store) *WorkSelectTool {
	return &WorkSelectTool{store: store}
}

func (t *WorkSelectTool) Name() string { return "work_select" }

func (t *WorkSelectTool) Description() string {
	return "Propose how the current request relates to one exact inspected historical run. Use observe for a read-only status/result question, or resume to continue that work after this interaction; the gateway validates and commits the proposal."
}

func (t *WorkSelectTool) Schema() ToolSchema {
	return ToolSchema{
		Type: "object",
		Properties: map[string]PropertyDef{
			"action": {Type: "string", Enum: []string{"observe", "resume"}, Description: "observe reads/reports prior state; resume schedules a validated continuation."},
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
	if current == nil || current.PersonID != scope.PersonID || current.TaskID != scope.TaskID || current.Status != "running" {
		return "", fmt.Errorf("current run is not eligible for work selection")
	}
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
	events, err := t.store.ListRunEvents(ctx, scope.ControlTenantID, scope.PersonID, scope.TaskID, scope.RunID, 50)
	if err != nil {
		return "", err
	}
	var previousAction, previousRunID string
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
		if previousAction == action && previousRunID == targetRunID {
			return workSelectionResult("proposed", action, targetRunID), nil
		}
		break
	}
	if previousRunID != "" {
		blocked, reason, err := t.store.RunSelectionEffectBoundary(ctx, scope.ControlTenantID, scope.PersonID, scope.RunID)
		if err != nil {
			return "", err
		}
		if blocked {
			return "", fmt.Errorf("the prior selection cannot be corrected after a material effect (%s); stop expanding it and ask the user how to proceed", reason)
		}
	}
	payload := map[string]interface{}{
		"action": action, "run_id": targetRunID, "task_id": target.TaskID,
		"target_workspace_id": target.WorkspaceID,
	}
	status := "proposed"
	if previousRunID != "" {
		status = "corrected"
		payload["correction_of"] = map[string]string{"action": previousAction, "run_id": previousRunID}
	}
	if _, err := t.store.AppendEvent(ctx, control.Event{
		TaskID: scope.TaskID, RunID: scope.RunID, Type: "work.selection", Visibility: "task",
		Payload: mustToolJSON(payload),
	}); err != nil {
		return "", err
	}
	return workSelectionResult(status, action, targetRunID), nil
}

func workSelectionResult(status, action, runID string) string {
	encoded, _ := json.Marshal(map[string]interface{}{
		"status": status, "action": action, "run_id": runID,
		"message": "The gateway will revalidate and commit this selection after the current Main turn. Do not perform work for the target run in the current execution scope.",
	})
	return string(encoded)
}

func mustToolJSON(value interface{}) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}
