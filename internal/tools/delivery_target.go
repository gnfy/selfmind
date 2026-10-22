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
	return "Route this active run's final result to an authenticated bound IM endpoint. Pass input_id when a [SelfMind live user input] asked to send the result there, or pass platform (for example weixin, wechat, telegram, or im) when the user's prompt explicitly asks to send or copy the result to their bound IM account."
}

func (t *SetDeliveryTargetTool) Schema() ToolSchema {
	return ToolSchema{
		Type:                 "object",
		AdditionalProperties: rejectAdditionalProperties(),
		Properties: map[string]PropertyDef{
			"input_id": {Type: "string", Description: "Server-issued id from the exact [SelfMind live user input] that explicitly requested final delivery here."},
			"platform": {Type: "string", Description: "Bound IM platform to receive the final result when explicitly requested by the user (for example weixin, wechat, telegram, or im). Never pass a user id or channel id."},
		},
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
	platform := strings.TrimSpace(taskStringArg(args, "platform"))
	if inputID == "" && platform == "" {
		return "", fmt.Errorf("input_id or platform is required")
	}
	var (
		target *control.RunDeliveryTarget
		err    error
	)
	if inputID != "" {
		target, err = t.store.SetRunDeliveryOverrideFromSteering(ContextFromArgs(args), scope.ControlTenantID, scope.PersonID, scope.RunID, inputID)
	} else {
		target, err = t.store.SetRunDeliveryOverrideToBoundPlatform(ContextFromArgs(args), scope.ControlTenantID, scope.PersonID, scope.RunID, platform)
	}
	if err != nil {
		return "", err
	}
	result, _ := json.Marshal(map[string]interface{}{
		"status": "updated", "run_id": target.RunID, "platform": target.Platform, "channel": target.Channel,
		"message": "The final result will be delivered only to this explicitly selected bound endpoint.",
	})
	return string(result), nil
}
