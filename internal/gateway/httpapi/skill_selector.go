package httpapi

import (
	"context"
	"crypto/sha256"
	"fmt"
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

	infos, err := tools.RankSkillCandidatesForTenant(identity.TenantID, userInput, 3, args)
	if err != nil {
		return kernel.SkillRuntimeContext{}
	}
	candidates := make([]kernel.SkillCandidateContext, 0, len(infos))
	for _, info := range infos {
		rel, err := filepath.Rel(info.Root, skillInfoMainPath(info))
		if err != nil {
			continue
		}
		candidates = append(candidates, kernel.SkillCandidateContext{
			Key:  control.SkillKey(identity.TenantID, info.Name, info.Scope, info.Source, info.Root, rel),
			Name: info.Name, Description: info.Description, Scope: info.Scope, Source: info.Source,
		})
	}
	return kernel.SkillRuntimeContext{Candidates: candidates}
}

func (c *RunCoordinator) resolveBoundSkill(ctx context.Context, identity *control.IdentityContext, task *control.Task, run *control.Run, binding *control.TaskSkillBinding, scope kernel.ToolInvocationScope, args map[string]interface{}) (*kernel.ActiveSkillContext, bool) {
	info, content, files, err := tools.ReadSkillPayloadForTenant(binding.ControlTenantID, binding.SkillName, "", args)
	if err != nil {
		c.recordSkillAvailability(ctx, task, run, binding, "unavailable", err.Error())
		return nil, false
	}
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
	digest := sha256.Sum256([]byte(content))
	versionHash := fmt.Sprintf("%x", digest[:])
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
	activation, err := c.srv.Control.ActivateSkill(ctx, control.ActivateSkillInput{
		IdentityTenantID: identity.TenantID, ControlTenantID: binding.ControlTenantID,
		PersonID: identity.PersonID, WorkspaceID: run.WorkspaceID, RunID: run.ID,
		WorkUnitID: run.WorkUnitID, ExecutionLane: "main", SkillKey: key, SkillName: info.Name,
		VersionHash: versionHash, ActivationSource: "task_binding", AttachmentMode: string(scope.AttachmentMode),
		ContentRef: mainPath, ContentBody: content, CreatedBy: "external_reconcile",
	})
	if err != nil {
		c.recordSkillAvailability(ctx, task, run, binding, "activation_failed", err.Error())
		return nil, false
	}
	_ = tools.MarkSkillUsed(binding.ControlTenantID, info.Name)
	_ = c.srv.Control.RecordTaskSkillBindingResolved(ctx, identity.TenantID, identity.PersonID, task.ID, versionHash)
	_, _ = c.srv.Control.AppendEvent(ctx, control.Event{
		TaskID: task.ID, RunID: run.ID, Type: "skill.activated", Visibility: "task", Channel: run.Channel,
		Payload: mustJSON(map[string]interface{}{
			"activation_id": activation.ID, "work_unit_id": activation.WorkUnitID,
			"skill_key": key, "name": info.Name, "version_hash": versionHash,
			"activation_source": "task_binding", "attachment_mode": scope.AttachmentMode,
		}),
		IdempotencyKey: "skill-activation:" + activation.ID,
	})
	return &kernel.ActiveSkillContext{
		ActivationID: activation.ID, WorkUnitID: activation.WorkUnitID, Key: key,
		Name: info.Name, VersionHash: versionHash, Scope: info.Scope, Source: info.Source,
		Body: content, LinkedFiles: files,
	}, true
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
