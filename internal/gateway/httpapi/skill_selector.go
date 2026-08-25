package httpapi

import (
	"context"
	"path/filepath"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/platform/log"
	"selfmind/internal/tools"
)

func (c *RunCoordinator) selectSkillRuntimeContext(ctx context.Context, identity *control.IdentityContext, task *control.Task, run *control.Run, attach taskAttach, userInput string) kernel.SkillRuntimeContext {
	if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil || task == nil || run == nil {
		return kernel.SkillRuntimeContext{}
	}
	scope, _ := kernel.ToolInvocationScopeFromContext(ctx)
	args := map[string]interface{}{
		"_tenant_id":        identity.TenantID,
		"_context":          ctx,
		"_invocation_scope": scope,
	}
	args = tools.WithSkillStorage(args, c.srv.SkillStorage)
	if explicit, ok := kernel.ExplicitSkillInvocationFromContext(ctx); ok {
		if active, resolved := c.resolveExplicitSkill(ctx, identity, task, run, explicit, scope, args); resolved {
			return kernel.SkillRuntimeContext{Active: active}
		}
		// Explicit user intent outranks automatic discovery. A stale, guarded,
		// or unavailable explicit Skill degrades to ordinary planning instead of
		// silently substituting another candidate.
		return kernel.SkillRuntimeContext{}
	}
	if attach.allowsTaskSkillBinding() {
		binding, err := c.srv.Control.GetTaskSkillBinding(ctx, identity.TenantID, identity.PersonID, task.ID)
		if err != nil {
			log.Warn("task skill binding lookup failed", "task_id", task.ID, "error", err)
		} else if binding != nil && binding.State != control.TaskSkillBindingReleased {
			if binding.State == control.TaskSkillBindingActive {
				if active, ok := c.resolveBoundSkill(ctx, identity, task, run, binding, scope, args); ok {
					return kernel.SkillRuntimeContext{Active: active}
				}
			}
			// A bound Skill that is guarded, suspended, unavailable, or mismatched
			// falls back to ordinary planning. Do not silently offer another Skill
			// for the same work unit and blur failure attribution.
			return kernel.SkillRuntimeContext{}
		}
	}

	candidates, _, _, ok := c.prepareSkillCandidateSnapshot(ctx, identity, run.ID, run.WorkUnitID, userInput, scope, args)
	if !ok {
		return kernel.SkillRuntimeContext{}
	}
	return kernel.SkillRuntimeContext{Candidates: candidates}
}

// prepareSkillCandidateSnapshot is the one catalog-to-ref transition used by
// initial context selection and update_plan work-unit changes. It returns all
// ranked candidates so the system-prompt renderer can report omissions, plus
// the prefix whose refs were actually issued and are safe to show elsewhere.
func (c *RunCoordinator) prepareSkillCandidateSnapshot(ctx context.Context, identity *control.IdentityContext, runID, workUnitID, query string, scope kernel.ToolInvocationScope, args map[string]interface{}) ([]kernel.SkillCandidateContext, string, kernel.SkillCatalogRenderReport, bool) {
	if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil || strings.TrimSpace(runID) == "" || strings.TrimSpace(workUnitID) == "" {
		return nil, "", kernel.SkillCatalogRenderReport{}, false
	}
	scope.RunID = runID
	scope.WorkUnitID = workUnitID
	resolvedArgs := make(map[string]interface{}, len(args)+2)
	for key, value := range args {
		resolvedArgs[key] = value
	}
	resolvedArgs["_context"] = ctx
	resolvedArgs["_invocation_scope"] = scope
	infos, err := tools.CatalogSkillCandidatesForTenant(identity.TenantID, query, resolvedArgs)
	if err != nil {
		return nil, "", kernel.SkillCatalogRenderReport{}, false
	}
	budget := kernel.DefaultRuntimeContextBudget()
	if c.srv.Gateway != nil {
		budget = c.srv.Gateway.RuntimeContextBudget()
	}
	candidates := make([]kernel.SkillCandidateContext, 0, len(infos))
	inputs := make([]control.IssueSkillCandidateRefInput, 0, len(infos))
	for _, info := range infos {
		pack, err := tools.ReadSkillPackageForTenant(identity.TenantID, info.Name, resolvedArgs)
		if err != nil {
			continue
		}
		resolved := pack.Info
		rel, err := filepath.Rel(resolved.Root, skillInfoMainPath(resolved))
		if err != nil {
			continue
		}
		key := control.SkillKey(identity.TenantID, resolved.Name, resolved.Scope, resolved.Source, resolved.Root, rel)
		input := control.IssueSkillCandidateRefInput{
			IdentityTenantID: identity.TenantID, ControlTenantID: identity.TenantID,
			PersonID: identity.PersonID, RunID: runID, WorkUnitID: workUnitID,
			SkillKey: key, SkillName: resolved.Name, VersionHash: pack.VersionHash,
			PackageHash: pack.PackageHash, DescriptionHash: pack.DescriptionHash,
		}
		candidateRef, err := control.SkillCandidateRefForInput(input)
		if err != nil {
			continue
		}
		candidates = append(candidates, kernel.SkillCandidateContext{
			CandidateRef: candidateRef, Key: key,
			Name: resolved.Name, Description: resolved.Description, Scope: resolved.Scope, Source: resolved.Source, Root: resolved.Root,
		})
		inputs = append(inputs, input)
	}
	catalog, report := kernel.RenderSkillCandidateCatalog(candidates, budget)
	for index := 0; index < report.Included; index++ {
		if _, err := c.srv.Control.IssueSkillCandidateRef(ctx, inputs[index]); err != nil {
			// Never render an opaque ref that the same work unit cannot resolve.
			// A partial snapshot would also make ranking/storage disagree, so fail
			// closed to ordinary planning for this turn.
			log.Warn("Skill candidate ref issuance failed", "run_id", runID, "work_unit_id", workUnitID, "name", inputs[index].SkillName, "error", err)
			return nil, "", kernel.SkillCatalogRenderReport{}, false
		}
	}
	return candidates, catalog, report, true
}

func (c *RunCoordinator) resolveBoundSkill(ctx context.Context, identity *control.IdentityContext, task *control.Task, run *control.Run, binding *control.TaskSkillBinding, scope kernel.ToolInvocationScope, args map[string]interface{}) (*kernel.ActiveSkillContext, bool) {
	pack, err := tools.ReadSkillPackageForTenant(binding.ControlTenantID, binding.SkillName, args)
	if err != nil {
		c.recordSkillAvailability(ctx, task, run, binding, "unavailable", err.Error())
		return nil, false
	}
	info := pack.Info
	if info.State != tools.SkillStateActive {
		_ = c.srv.Control.SetTaskSkillBindingState(ctx, identity.TenantID, identity.PersonID, task.ID, control.TaskSkillBindingSuspended, "skill is not active")
		c.recordSkillAvailability(ctx, task, run, binding, "suspended", "skill is not active")
		return nil, false
	}
	mainPath := skillInfoMainPath(info)
	rel, err := filepath.Rel(info.Root, mainPath)
	if err != nil {
		return nil, false
	}
	key := control.SkillKey(binding.ControlTenantID, info.Name, info.Scope, info.Source, info.Root, rel)
	if key != binding.SkillKey {
		_ = c.srv.Control.SetTaskSkillBindingState(ctx, identity.TenantID, identity.PersonID, task.ID, control.TaskSkillBindingSuspended, "skill resolution identity changed")
		c.recordSkillAvailability(ctx, task, run, binding, "resolution_mismatch", "a different root/path now wins for this name")
		return nil, false
	}
	versionHash := pack.VersionHash
	guard, err := c.srv.Control.MatchSkillFailureGuardForWorkUnit(ctx, identity.TenantID, key, versionHash, run.ID, run.WorkUnitID)
	if err != nil {
		c.recordSkillAvailability(ctx, task, run, binding, "guard_lookup_failed", err.Error())
		return nil, false
	}
	if guard != nil {
		occurrences, recordErr := c.srv.Control.RecordSkillFailureGuardMatch(ctx, *guard)
		if recordErr != nil {
			log.Warn("skill failure guard match record failed", "task_id", task.ID, "error", recordErr)
		}
		if occurrences >= 2 {
			_ = c.srv.Control.SetTaskSkillBindingState(ctx, identity.TenantID, identity.PersonID, task.ID, control.TaskSkillBindingSuspended, "repeated known failure guard")
		}
		_, _ = c.srv.Control.AppendEvent(ctx, control.Event{
			TaskID: task.ID, RunID: run.ID, Type: "skill.guard.matched", Visibility: "task", Channel: run.Channel,
			Payload: mustJSON(map[string]interface{}{
				"skill_key": key, "name": info.Name, "version_hash": versionHash,
				"failure_signature": guard.FailureSignature, "failed_step_id": guard.FailedStepID,
				"error_category": guard.ErrorCategory, "occurrence_count": occurrences,
				"action": "ordinary_planning_without_skill",
			}),
			IdempotencyKey: "skill-guard-match:" + run.ID + ":" + guard.FailureSignature,
		})
		return nil, false
	}
	budget := kernel.DefaultRuntimeContextBudget()
	if c.srv.Gateway != nil {
		budget = c.srv.Gateway.RuntimeContextBudget()
	}
	activated, err := tools.ActivateSkillPackage(ctx, c.srv.Control, pack, tools.ActivateSkillPackageInput{
		IdentityTenantID: identity.TenantID, ControlTenantID: binding.ControlTenantID,
		PersonID: identity.PersonID, WorkspaceID: run.WorkspaceID, RunID: run.ID,
		WorkUnitID: run.WorkUnitID, ExecutionLane: "main", SkillKey: key,
		ActivationSource: "task_binding", AttachmentMode: string(scope.AttachmentMode),
		ContentRef: mainPath, CreatedBy: "external_reconcile", Budget: budget,
	})
	if err != nil {
		c.recordSkillAvailability(ctx, task, run, binding, "activation_failed", err.Error())
		return nil, false
	}
	activation := activated.Activation
	_ = tools.MarkSkillUsed(binding.ControlTenantID, info.Name, args)
	_ = c.srv.Control.RecordTaskSkillBindingResolved(ctx, identity.TenantID, identity.PersonID, task.ID, versionHash)
	_, _ = c.srv.Control.AppendEvent(ctx, control.Event{
		TaskID: task.ID, RunID: run.ID, Type: "skill.activated", Visibility: "task", Channel: run.Channel,
		Payload: mustJSON(map[string]interface{}{
			"activation_id": activation.ID, "work_unit_id": activation.WorkUnitID,
			"skill_key": key, "name": info.Name, "version_hash": versionHash,
			"package_hash": activation.PackageHash, "delivery_contract_version": activation.DeliveryContractVersion,
			"delivery_mode": activation.DeliveryMode, "delivered_main_hash": activation.DeliveredMainHash,
			"delivered_main_bytes": activation.DeliveredMainBytes,
			"activation_source":    "task_binding", "attachment_mode": scope.AttachmentMode,
		}),
		IdempotencyKey: "skill-activation:" + activation.ID,
	})
	return activated.Context, true
}

func (c *RunCoordinator) resolveExplicitSkill(ctx context.Context, identity *control.IdentityContext, task *control.Task, run *control.Run, explicit kernel.ExplicitSkillInvocation, scope kernel.ToolInvocationScope, args map[string]interface{}) (*kernel.ActiveSkillContext, bool) {
	pack, err := tools.ReadSkillPackageForTenant(identity.TenantID, explicit.Name, args)
	if err != nil || pack.Info.State != tools.SkillStateActive {
		reason := "skill is not active"
		if err != nil {
			reason = err.Error()
		}
		c.recordExplicitSkillAvailability(ctx, task, run, explicit, "unavailable", reason)
		return nil, false
	}
	rel, err := filepath.Rel(pack.Info.Root, skillInfoMainPath(pack.Info))
	if err != nil {
		c.recordExplicitSkillAvailability(ctx, task, run, explicit, "unavailable", err.Error())
		return nil, false
	}
	key := control.SkillKey(identity.TenantID, pack.Info.Name, pack.Info.Scope, pack.Info.Source, pack.Info.Root, rel)
	if key != explicit.SkillKey || pack.VersionHash != explicit.VersionHash || pack.PackageHash != explicit.PackageHash {
		c.recordExplicitSkillAvailability(ctx, task, run, explicit, "stale", "the resolved Skill package changed after slash resolution")
		return nil, false
	}
	guard, err := c.srv.Control.MatchSkillFailureGuardForWorkUnit(ctx, identity.TenantID, key, pack.VersionHash, run.ID, run.WorkUnitID)
	if err != nil || guard != nil {
		reason := "a known failure guard matches this work unit"
		if err != nil {
			reason = err.Error()
		}
		c.recordExplicitSkillAvailability(ctx, task, run, explicit, "guarded", reason)
		return nil, false
	}
	budget := kernel.DefaultRuntimeContextBudget()
	if c.srv.Gateway != nil {
		budget = c.srv.Gateway.RuntimeContextBudget()
	}
	activated, err := tools.ActivateSkillPackage(ctx, c.srv.Control, pack, tools.ActivateSkillPackageInput{
		IdentityTenantID: identity.TenantID, ControlTenantID: identity.TenantID,
		PersonID: identity.PersonID, WorkspaceID: run.WorkspaceID, RunID: run.ID,
		WorkUnitID: run.WorkUnitID, ExecutionLane: "main", SkillKey: key,
		ActivationSource: "slash", AttachmentMode: string(scope.AttachmentMode),
		ContentRef: skillInfoMainPath(pack.Info), CreatedBy: "external_reconcile", Budget: budget,
	})
	if err != nil {
		c.recordExplicitSkillAvailability(ctx, task, run, explicit, "activation_failed", err.Error())
		return nil, false
	}
	activation := activated.Activation
	_ = tools.MarkSkillUsed(identity.TenantID, pack.Info.Name, args)
	_, _ = c.srv.Control.AppendEvent(ctx, control.Event{
		TaskID: task.ID, RunID: run.ID, Type: "skill.activated", Visibility: "task", Channel: run.Channel,
		Payload: mustJSON(map[string]interface{}{
			"activation_id": activation.ID, "work_unit_id": activation.WorkUnitID,
			"skill_key": key, "name": pack.Info.Name, "version_hash": pack.VersionHash,
			"package_hash": activation.PackageHash, "delivery_contract_version": activation.DeliveryContractVersion,
			"delivery_mode": activation.DeliveryMode, "delivered_main_hash": activation.DeliveredMainHash,
			"delivered_main_bytes": activation.DeliveredMainBytes,
			"activation_source":    "slash", "attachment_mode": scope.AttachmentMode,
		}),
		IdempotencyKey: "skill-activation:" + activation.ID,
	})
	return activated.Context, true
}

func (c *RunCoordinator) recordExplicitSkillAvailability(ctx context.Context, task *control.Task, run *control.Run, explicit kernel.ExplicitSkillInvocation, state, reason string) {
	_, _ = c.srv.Control.AppendEvent(ctx, control.Event{
		TaskID: task.ID, RunID: run.ID, Type: "skill.unavailable", Visibility: "task", Channel: run.Channel,
		Payload: mustJSON(map[string]string{
			"skill_key": explicit.SkillKey, "name": explicit.Name,
			"version_hash": explicit.VersionHash, "package_hash": explicit.PackageHash,
			"state": state, "reason": strings.TrimSpace(reason),
		}),
	})
}

func (c *RunCoordinator) recordSkillAvailability(ctx context.Context, task *control.Task, run *control.Run, binding *control.TaskSkillBinding, state, reason string) {
	_, _ = c.srv.Control.AppendEvent(ctx, control.Event{
		TaskID: task.ID, RunID: run.ID, Type: "skill.unavailable", Visibility: "task", Channel: run.Channel,
		Payload: mustJSON(map[string]string{
			"skill_key": binding.SkillKey, "name": binding.SkillName,
			"state": state, "reason": strings.TrimSpace(reason),
		}),
	})
}

func skillInfoMainPath(info tools.SkillInfo) string {
	if info.Format == "dir" {
		return filepath.Join(info.Path, "SKILL.md")
	}
	return info.Path
}
