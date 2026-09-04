package tools

import (
	"encoding/json"
	"fmt"
	"strings"

	"selfmind/internal/control"
)

// QueueUserInputTool is the Main-facing half of active-run steering. Main calls
// it only when a server-issued live input is independent of the current
// objective. The model never repeats or chooses the queued prose; input_id
// addresses the exact durable mailbox row, and the authenticated invocation
// scope supplies all ownership authority.
type QueueUserInputTool struct {
	store *control.Store
}

func NewQueueUserInputTool(store *control.Store) *QueueUserInputTool {
	return &QueueUserInputTool{store: store}
}

func (t *QueueUserInputTool) Name() string { return "queue_user_input" }

func (t *QueueUserInputTool) Description() string {
	return "Queue one server-issued live user input outside the active objective. Omit resumes_run_id for independent new work, or provide an exact inspected resumable run for a historical continuation."
}

func (t *QueueUserInputTool) Schema() ToolSchema {
	return ToolSchema{
		Type: "object",
		Properties: map[string]PropertyDef{
			"input_id": {
				Type:        "string",
				Description: "Server-issued id from a [SelfMind live user input] block. Never invent or reuse an id.",
			},
			"resumes_run_id": {
				Type:        "string",
				Description: "Optional exact resumable run_id when this input clearly continues other historical work. Omit for independent new work.",
			},
		},
		Required: []string{"input_id"},
	}
}

func (t *QueueUserInputTool) Metadata() ToolMetadata {
	return ToolMetadata{Exposure: ToolExposureDirect, RiskLevel: ToolRiskLow, Category: "task"}
}

func (t *QueueUserInputTool) Execute(args map[string]interface{}) (string, error) {
	if t == nil || t.store == nil {
		return "", fmt.Errorf("work queue is unavailable")
	}
	scope, ok := InvocationScopeFromArgs(args)
	if !ok || strings.TrimSpace(scope.ControlTenantID) == "" || strings.TrimSpace(scope.PersonID) == "" || strings.TrimSpace(scope.RunID) == "" {
		return "", fmt.Errorf("authenticated run scope is required")
	}
	inputID := strings.TrimSpace(taskStringArg(args, "input_id"))
	if inputID == "" {
		return "", fmt.Errorf("input_id is required")
	}
	resumesRunID := strings.TrimSpace(taskStringArg(args, "resumes_run_id"))
	var queued *control.QueuedTask
	var err error
	if resumesRunID != "" {
		queued, err = t.store.QueueSteeringAsContinuation(
			ContextFromArgs(args), scope.ControlTenantID, scope.PersonID, scope.RunID, inputID, resumesRunID,
		)
	} else {
		queued, err = t.store.QueueSteeringAsIndependent(
			ContextFromArgs(args), scope.ControlTenantID, scope.PersonID, scope.RunID, inputID,
		)
	}
	if err != nil {
		return "", err
	}
	result, _ := json.Marshal(map[string]interface{}{
		"status": "queued", "input_id": inputID, "queue_id": queued.ID,
		"resumes_run_id": resumesRunID,
		"message":        "User input queued outside the active plan; the current plan is unchanged.",
	})
	return string(result), nil
}
