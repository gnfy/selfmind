package tools

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
)

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
					"candidate_ref": {Type: "string", Description: "Opaque current-work-unit reference shown in the Skill candidate catalogue. Use this for model-selected Skills."},
					"name":          {Type: "string", Description: "Existing active skill name. Omit only to resolve the current work unit's related task binding."},
					"reason":        {Type: "string", Description: "Short reason this skill applies to the current work unit."},
					"work_unit_id":  {Type: "string", Description: "Optional stable work-unit id returned in plan events. Omit to select the current in-progress work unit."},
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
	candidateRef, _ := args["candidate_ref"].(string)
	reason, _ := args["reason"].(string)
	workUnitID, _ := args["work_unit_id"].(string)
	tenantID := skillStorageTenantID(args)
	activationSource := "model"
	expectedSkillKey := ""
	var issuedRef *control.SkillCandidateRef
	if strings.TrimSpace(candidateRef) != "" {
		if strings.TrimSpace(workUnitID) == "" {
			unit, err := t.store.CurrentRunWorkUnit(ContextFromArgs(args), scope.ControlTenantID, scope.RunID)
			if err != nil {
				return "", err
			}
			if unit == nil {
				return "", fmt.Errorf("current run has no selectable work unit")
			}
			workUnitID = unit.ID
		}
		var err error
		issuedRef, err = t.store.ResolveSkillCandidateRef(ContextFromArgs(args), scope.ControlTenantID,
			scope.PersonID, scope.RunID, strings.TrimSpace(workUnitID), strings.TrimSpace(candidateRef))
		if err != nil {
			return "", err
		}
		if issuedRef == nil {
			return "", newStableToolError(errors.New("candidate ref is not scoped to the current work unit"), "candidate_unknown", "stale_precondition",
				"That Skill candidate reference was not issued for the current work unit.",
				"Use a candidate_ref from the current Skill catalogue or refresh it with skills_list.")
		}
		tenantID = issuedRef.ControlTenantID
		name = issuedRef.SkillName
		expectedSkillKey = issuedRef.SkillKey
	} else if strings.TrimSpace(name) == "" {
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
	} else {
		return "", newStableToolError(errors.New("model Skill selection omitted candidate_ref"), "candidate_ref_required", "stale_precondition",
			"Model-selected Skills must use the candidate_ref issued for this work unit, not a display name.",
			"Copy candidate_ref from the current Skill catalogue or continue without a Skill.")
	}
	pack, err := ReadSkillPackageForTenant(tenantID, name, args)
	if err != nil {
		var notFound *skillNotFoundError
		if errors.As(err, &notFound) {
			return "", staleSkillSelectionError(err, tenantID, name, reason, expectedSkillKey != "", args)
		}
		return "", err
	}
	info := pack.Info
	if info.State != SkillStateActive {
		return "", fmt.Errorf("skill %q is not active", info.Name)
	}
	mainPath := skillMainFilePath(info)
	relativePath, err := filepath.Rel(info.Root, mainPath)
	if err != nil {
		return "", fmt.Errorf("resolve skill identity: %w", err)
	}
	skillKey := control.SkillKey(tenantID, info.Name, info.Scope, info.Source, info.Root, relativePath)
	if expectedSkillKey != "" && skillKey != expectedSkillKey {
		if issuedRef != nil {
			_, _ = t.store.RecordSkillCandidateRefUse(ContextFromArgs(args), issuedRef.CandidateRef, "stale", false)
			return "", newStableToolError(errors.New("candidate Skill resolution identity changed"), "candidate_stale", "stale_precondition",
				"A different Skill root now wins precedence for this name, so the issued candidate identity is stale.",
				"Refresh candidates with skills_list and decide again; do not use the same-named replacement through the old reference.")
		}
		return "", newStableToolError(errors.New("related task Skill resolution identity changed"), "candidate_stale", "stale_precondition",
			"The related task's bound Skill now resolves to a different root or source.",
			"Continue this work unit with ordinary planning; do not guess or select the same-named replacement.")
	}
	drifted := false
	driftNotice := ""
	if issuedRef != nil {
		if pack.DescriptionHash != issuedRef.DescriptionHash {
			_, _ = t.store.RecordSkillCandidateRefUse(ContextFromArgs(args), issuedRef.CandidateRef, "stale", false)
			return "", newStableToolError(errors.New("candidate description hash changed"), "candidate_stale", "stale_precondition",
				"The Skill description changed after this candidate was presented, so the routing decision is stale.",
				"Refresh candidates with skills_list and decide again; do not guess from the old reference.")
		}
		if pack.PackageHash != issuedRef.PackageHash || pack.VersionHash != issuedRef.VersionHash {
			if issuedRef.DriftCount >= 1 {
				_, _ = t.store.RecordSkillCandidateRefUse(ContextFromArgs(args), issuedRef.CandidateRef, "stale", false)
				return "", newStableToolError(errors.New("candidate package drifted more than once"), "candidate_stale", "stale_precondition",
					"The Skill package changed more than once after this candidate was presented.",
					"Abandon this candidate and continue the work unit without a Skill, or refresh candidates before any side effect.")
			}
			drifted = true
			driftNotice = "The description is unchanged but the package changed once after candidate presentation. The current package has been re-delivered before any Skill-guided side effect."
		}
	}
	runtimeBudget := kernel.DefaultRuntimeContextBudget()
	if bundle, ok := kernel.RuntimeContextBundleFromContext(ContextFromArgs(args)); ok && bundle.Budget.SkillMainBytes > 0 {
		runtimeBudget = bundle.Budget
	}
	activated, err := ActivateSkillPackage(ContextFromArgs(args), t.store, pack, ActivateSkillPackageInput{
		IdentityTenantID: scope.ControlTenantID, ControlTenantID: tenantID,
		PersonID: scope.PersonID, WorkspaceID: scope.WorkspaceID, RunID: scope.RunID,
		WorkUnitID: strings.TrimSpace(workUnitID), ExecutionLane: scope.ExecutionLane,
		SkillKey: skillKey, ActivationSource: activationSource, AttachmentMode: scope.AttachmentMode,
		ContentRef: mainPath, CreatedBy: "external_reconcile", Budget: runtimeBudget,
	})
	if err != nil {
		return "", err
	}
	activation := activated.Activation
	active := activated.Context
	delivery := activated.Delivery
	if driftNotice != "" && activation.PackageHash != pack.PackageHash {
		driftNotice = "This work unit already has an immutable Skill activation. Its pinned package was re-delivered; the newer filesystem package applies only to a future activation."
	}
	if issuedRef != nil {
		state := "selected"
		if drifted {
			state = "stale"
		}
		if _, err := t.store.RecordSkillCandidateRefUse(ContextFromArgs(args), issuedRef.CandidateRef, state, drifted); err != nil {
			return "", err
		}
	}
	_ = MarkSkillUsed(tenantID, info.Name, args)
	out := map[string]interface{}{
		"success": true, "activation_id": activation.ID, "work_unit_id": activation.WorkUnitID,
		"work_unit_sequence": activation.WorkUnitSequence, "skill_key": active.Key, "name": active.Name,
		"version_hash": active.VersionHash, "package_hash": activation.PackageHash,
		"reason": strings.TrimSpace(reason), "instructions": delivery.Content,
		"linked_files": active.LinkedFiles, "delivery_mode": delivery.Mode,
		"delivery_contract_version": delivery.ContractVersion,
		"delivered_main_hash":       delivery.DeliveredHash, "delivered_main_bytes": delivery.DeliveredBytes,
		"notice": "These instructions apply only to this work unit. Use skill_fallback if they are wrong or unusable; then replan without selecting another skill for this work unit.",
	}
	if driftNotice != "" {
		out["candidate_notice"] = driftNotice
		out["candidate_version_hash"] = issuedRef.VersionHash
		out["candidate_package_hash"] = issuedRef.PackageHash
		out["selected_version_hash"] = active.VersionHash
		out["selected_package_hash"] = activation.PackageHash
		if activation.PackageHash != pack.PackageHash {
			out["resolved_package_hash"] = pack.PackageHash
		}
	}
	kernel.EmitAgentEvent(kernel.EventChannelFromContext(ContextFromArgs(args)), kernel.AgentEvent{
		Type: "skill.activated",
		Payload: map[string]interface{}{
			"activation_id": activation.ID, "work_unit_id": activation.WorkUnitID,
			"skill_key": active.Key, "name": active.Name, "version_hash": active.VersionHash,
			"package_hash": activation.PackageHash, "delivery_contract_version": delivery.ContractVersion,
			"delivery_mode": delivery.Mode, "delivered_main_hash": delivery.DeliveredHash,
			"delivered_main_bytes": delivery.DeliveredBytes,
			"source":               info.Source, "scope": info.Scope, "root": info.Root,
			"activation_source": activation.ActivationSource, "attachment_mode": activation.AttachmentMode,
		},
	})
	data, _ := json.Marshal(out)
	return string(data), nil
}

func staleSkillSelectionError(cause error, tenantID, name, reason string, taskBound bool, args map[string]interface{}) error {
	if taskBound {
		return newStableToolError(
			cause,
			"candidate_stale",
			"stale_precondition",
			"The related task's bound Skill is no longer available.",
			"Continue this work unit with ordinary planning; do not guess or select a replacement Skill.",
		)
	}
	query := strings.TrimSpace(reason)
	if query == "" {
		query = strings.TrimSpace(name)
	}
	infos, _ := RankSkillCandidatesForTenant(tenantID, query, 3, args)
	candidates := make([]string, 0, len(infos))
	for _, info := range infos {
		if info.State == SkillStateActive {
			candidates = append(candidates, info.Name)
		}
	}
	safeMessage := fmt.Sprintf("The requested Skill %q is no longer available.", strings.TrimSpace(name))
	if len(candidates) > 0 {
		safeMessage += " Current candidates: " + strings.Join(candidates, ", ") + "."
	} else {
		safeMessage += " No current candidate matches this work unit."
	}
	return newStableToolError(
		cause,
		"candidate_stale",
		"stale_precondition",
		safeMessage,
		"Select only a listed current candidate, or continue the work unit without a Skill.",
	)
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
					"failed_tool_call_id":    {Type: "string", Description: "Optional call id of the actual failed tool invocation. When present, it must match daemon-observed failure evidence."},
					"error_category":         {Type: "string", Enum: control.SkillRepairErrorCategories(), Description: "Optional stable Skill-defect category. Unknown, transient, provider, environment, approval, and cancellation failures never authorize automatic repair."},
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
	failedToolCallID := taskStringArg(args, "failed_tool_call_id")
	category, _ := control.NormalizeSkillRepairErrorCategory(taskStringArg(args, "error_category"))
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
			"version_hash": activation.VersionHash, "reason": reason,
			"failure_signature": fmt.Sprintf("%x", sig[:]), "failed_step_id": failedStep,
			"failed_tool_call_id": failedToolCallID,
			"error_category":      category, "normalized_input_shape": inputShape,
		},
	})
	data, _ := json.Marshal(map[string]interface{}{
		"success": true, "activation_id": activation.ID, "work_unit_id": activation.WorkUnitID,
		"fallback": true, "instruction": "Replan and complete this work unit with ordinary tools. Do not select another skill for it.",
	})
	return string(data), nil
}
