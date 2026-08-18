package tools

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
)

const maxAutoSkillContextBytes = 8 * 1024

type SkillSelectTool struct {
	BaseTool
	store *control.Store
}

func NewSkillSelectTool(store *control.Store) *SkillSelectTool {
	return &SkillSelectTool{
		BaseTool: BaseTool{
			name:        "skill_select",
			description: "Activate one existing skill for the current work unit. Omit name to resolve that work unit's related task default binding. This records real execution attribution; skill_view only inspects content. A work unit may activate at most one skill, and after fallback it must finish with ordinary planning.",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"name":         {Type: "string", Description: "Existing active skill name. Omit only to resolve the current work unit's related task binding."},
					"reason":       {Type: "string", Description: "Short reason this skill applies to the current work unit."},
					"work_unit_id": {Type: "string", Description: "Optional stable work-unit id returned in plan events. Omit to select the current in-progress work unit."},
				},
				Required: []string{"reason"},
			},
			metadata: ToolMetadata{Category: "task", RiskLevel: ToolRiskLow},
		},
		store: store,
	}
}

func (t *SkillSelectTool) Execute(args map[string]interface{}) (string, error) {
	if t == nil || t.store == nil {
		return "", fmt.Errorf("skill lifecycle store is unavailable")
	}
	scope, ok := InvocationScopeFromArgs(args)
	if !ok || strings.TrimSpace(scope.RunID) == "" || strings.TrimSpace(scope.PersonID) == "" {
		return "", fmt.Errorf("skill_select requires an authenticated run scope")
	}
	name, _ := args["name"].(string)
	reason, _ := args["reason"].(string)
	workUnitID, _ := args["work_unit_id"].(string)
	tenantID := skillStorageTenantID(args)
	activationSource := "model"
	expectedSkillKey := ""
	if strings.TrimSpace(name) == "" {
		binding, err := t.store.TaskSkillBindingForWorkUnit(ContextFromArgs(args), scope.ControlTenantID,
			scope.PersonID, scope.RunID, strings.TrimSpace(workUnitID))
		if err != nil {
			return "", err
		}
		if binding == nil || binding.State != control.TaskSkillBindingActive {
			return "", fmt.Errorf("current work unit has no active related-task Skill binding; continue without a skill")
		}
		tenantID = binding.ControlTenantID
		name = binding.SkillName
		expectedSkillKey = binding.SkillKey
		activationSource = "task_binding"
	}
	info, content, files, err := ReadSkillPayloadForTenant(tenantID, name, "", args)
	if err != nil {
		return "", err
	}
	if info.State != SkillStateActive {
		return "", fmt.Errorf("skill %q is not active", info.Name)
	}
	mainPath := skillMainFilePath(info)
	relativePath, err := filepath.Rel(info.Root, mainPath)
	if err != nil {
		return "", fmt.Errorf("resolve skill identity: %w", err)
	}
	digest := sha256.Sum256([]byte(content))
	versionHash := fmt.Sprintf("%x", digest[:])
	skillKey := control.SkillKey(tenantID, info.Name, info.Scope, info.Source, info.Root, relativePath)
	if expectedSkillKey != "" && skillKey != expectedSkillKey {
		return "", fmt.Errorf("related task Skill resolution identity changed; continue without a skill")
	}
	activation, err := t.store.ActivateSkill(ContextFromArgs(args), control.ActivateSkillInput{
		IdentityTenantID: scope.ControlTenantID,
		ControlTenantID:  tenantID,
		PersonID:         scope.PersonID,
		WorkspaceID:      scope.WorkspaceID,
		RunID:            scope.RunID,
		WorkUnitID:       strings.TrimSpace(workUnitID),
		ExecutionLane:    scope.ExecutionLane,
		SkillKey:         skillKey,
		SkillName:        info.Name,
		VersionHash:      versionHash,
		ActivationSource: activationSource,
		AttachmentMode:   scope.AttachmentMode,
		ContentRef:       mainPath,
		ContentBody:      content,
		CreatedBy:        "external_reconcile",
	})
	if err != nil {
		return "", err
	}
	_ = MarkSkillUsed(tenantID, info.Name, args)
	bounded, truncated := truncateUTF8ByBytes(content, maxAutoSkillContextBytes)
	out := map[string]interface{}{
		"success": true, "activation_id": activation.ID, "work_unit_id": activation.WorkUnitID,
		"work_unit_sequence": activation.WorkUnitSequence, "skill_key": skillKey, "name": info.Name,
		"version_hash": versionHash, "reason": strings.TrimSpace(reason), "instructions": bounded,
		"linked_files": files, "truncated": truncated,
		"notice": "These instructions apply only to this work unit. Use skill_fallback if they are wrong or unusable; then replan without selecting another skill for this work unit.",
	}
	kernel.EmitAgentEvent(kernel.EventChannelFromContext(ContextFromArgs(args)), kernel.AgentEvent{
		Type: "skill.activated",
		Payload: map[string]interface{}{
			"activation_id": activation.ID, "work_unit_id": activation.WorkUnitID,
			"skill_key": skillKey, "name": info.Name, "version_hash": versionHash,
			"source": info.Source, "scope": info.Scope, "root": info.Root,
			"activation_source": activation.ActivationSource, "attachment_mode": activation.AttachmentMode,
		},
	})
	data, _ := json.Marshal(out)
	return string(data), nil
}

type SkillFallbackTool struct {
	BaseTool
	store *control.Store
}

func NewSkillFallbackTool(store *control.Store) *SkillFallbackTool {
	return &SkillFallbackTool{
		BaseTool: BaseTool{
			name:        "skill_fallback",
			description: "Stop using the active skill for the current work unit after attributable mismatch or failure, record a negative guard, and continue the same work unit with ordinary AI planning. Do not select a replacement skill in this work unit.",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"reason":                 {Type: "string", Description: "Concise evidence-backed reason the active skill cannot continue."},
					"failed_step_id":         {Type: "string", Description: "Optional stable/concise failed procedure step."},
					"error_category":         {Type: "string", Description: "Optional normalized category such as command_failed, stale_precondition, or environment_mismatch."},
					"normalized_input_shape": {Type: "string", Description: "Optional non-sensitive input shape used to avoid repeating the same failed step."},
					"work_unit_id":           {Type: "string", Description: "Optional current work-unit id."},
				},
				Required: []string{"reason"},
			},
			metadata: ToolMetadata{Category: "task", RiskLevel: ToolRiskLow},
		},
		store: store,
	}
}

func (t *SkillFallbackTool) Execute(args map[string]interface{}) (string, error) {
	if t == nil || t.store == nil {
		return "", fmt.Errorf("skill lifecycle store is unavailable")
	}
	scope, ok := InvocationScopeFromArgs(args)
	if !ok || scope.RunID == "" {
		return "", fmt.Errorf("skill_fallback requires an authenticated run scope")
	}
	reason := taskStringArg(args, "reason")
	failedStep := taskStringArg(args, "failed_step_id")
	category := taskStringArg(args, "error_category")
	inputShape := taskStringArg(args, "normalized_input_shape")
	workUnitID := taskStringArg(args, "work_unit_id")
	sigRaw, _ := json.Marshal([]string{strings.TrimSpace(failedStep), strings.TrimSpace(category), strings.TrimSpace(inputShape), strings.TrimSpace(reason)})
	sig := sha256.Sum256(sigRaw)
	activation, err := t.store.FallbackCurrentSkill(ContextFromArgs(args), control.SkillFallbackInput{
		IdentityTenantID: scope.ControlTenantID, RunID: scope.RunID, WorkUnitID: workUnitID,
		ExecutionLane: scope.ExecutionLane, Reason: reason, FailureSignature: fmt.Sprintf("%x", sig[:]),
		FailedStepID: failedStep, ErrorCategory: category, NormalizedInputShape: inputShape,
	})
	if err != nil {
		return "", err
	}
	kernel.EmitAgentEvent(kernel.EventChannelFromContext(ContextFromArgs(args)), kernel.AgentEvent{
		Type: "skill.fallback",
		Payload: map[string]interface{}{
			"activation_id": activation.ID, "work_unit_id": activation.WorkUnitID,
			"skill_key": activation.SkillKey, "name": activation.SkillName,
			"version_hash": activation.VersionHash, "reason": reason, "error_category": category,
		},
	})
	data, _ := json.Marshal(map[string]interface{}{
		"success": true, "activation_id": activation.ID, "work_unit_id": activation.WorkUnitID,
		"fallback": true, "instruction": "Replan and complete this work unit with ordinary tools. Do not select another skill for it.",
	})
	return string(data), nil
}
