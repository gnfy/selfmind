package tools

import (
	"encoding/json"
	"fmt"
	"strings"

	"selfmind/internal/control"
)

// SetDeliveryTargetTool lets Main honor an explicit live request such as
// "send the final result here" without accepting model-supplied endpoint
// identifiers. input_id resolves the authenticated steering route in control.
type SetDeliveryTargetTool struct {
	store *control.Store
}

func NewSetDeliveryTargetTool(store *control.Store) *SetDeliveryTargetTool {
	return &SetDeliveryTargetTool{store: store}
}

func (t *SetDeliveryTargetTool) Name() string { return "set_delivery_target" }

func (t *SetDeliveryTargetTool) Description() string {
	return "Move this active run's final result to the endpoint that sent one server-issued live input. Use only when that input explicitly asks to send the final result here; never invent an input_id or endpoint."
}

func (t *SetDeliveryTargetTool) Schema() ToolSchema {
	return ToolSchema{
		Type: "object",
		Properties: map[string]PropertyDef{
			"input_id": {Type: "string", Description: "Server-issued id from the exact [SelfMind live user input] that explicitly requested final delivery here."},
		},
		Required: []string{"input_id"},
	}
}

func (t *SetDeliveryTargetTool) Metadata() ToolMetadata {
	return ToolMetadata{Exposure: ToolExposureDirect, RiskLevel: ToolRiskLow, Category: "task"}
}

func (t *SetDeliveryTargetTool) Execute(args map[string]interface{}) (string, error) {
	if t == nil || t.store == nil {
		return "", fmt.Errorf("delivery targeting is unavailable")
	}
	scope, ok := InvocationScopeFromArgs(args)
	if !ok || strings.TrimSpace(scope.ControlTenantID) == "" || strings.TrimSpace(scope.PersonID) == "" || strings.TrimSpace(scope.RunID) == "" {
		return "", fmt.Errorf("authenticated run scope is required")
	}
	inputID := strings.TrimSpace(taskStringArg(args, "input_id"))
	if inputID == "" {
		return "", fmt.Errorf("input_id is required")
	}
	target, err := t.store.SetRunDeliveryOverrideFromSteering(ContextFromArgs(args), scope.ControlTenantID, scope.PersonID, scope.RunID, inputID)
	if err != nil {
		return "", err
	}
	result, _ := json.Marshal(map[string]interface{}{
		"status": "updated", "run_id": target.RunID, "platform": target.Platform, "channel": target.Channel,
		"message": "The final result will be delivered only to this explicitly selected bound endpoint.",
	})
	return string(result), nil
}
