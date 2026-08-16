package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
)

func TestSkillSelectAndFallbackUseDurableActivation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Alice")
	task, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "deploy", Channel: "cli"})
	run, _ := store.StartRun(ctx, task, "cli", "deploy safely")

	manage := NewSkillManageTool()
	for _, name := range []string{"deploy-safe", "deploy-other"} {
		if _, err := manage.Execute(directSkillMutationArgs(map[string]interface{}{
			"action": "create", "name": name,
			"description": "deploy safely", "content": "Check preconditions, deploy, then verify.",
			"source": SkillSourceAgentCreated,
		})); err != nil {
			t.Fatal(err)
		}
	}
	events := make(chan string, 4)
	toolCtx := kernel.WithEventChannel(ctx, events)
	scope := kernel.ToolInvocationScope{
		ControlTenantID: identity.TenantID, PersonID: identity.PersonID, RunID: run.ID,
		WorkUnitID: run.WorkUnitID, ExecutionLane: "main", AttachmentMode: "continuation",
	}
	args := map[string]interface{}{
		"name": "deploy-safe", "reason": "matches deployment work",
		"_context": toolCtx, "_invocation_scope": scope,
	}
	result, err := NewSkillSelectTool(store).Execute(args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"instructions":`) || !strings.Contains(result, `Check preconditions`) || !strings.Contains(result, `"activation_id"`) {
		t.Fatalf("selection did not return bounded active instructions: %s", result)
	}
	raw := <-events
	event, ok := kernel.DecodeAgentEvent(raw)
	if !ok || event.Type != "skill.activated" {
		t.Fatalf("activation event=%+v ok=%v", event, ok)
	}

	args["name"] = "deploy-other"
	if _, err := NewSkillSelectTool(store).Execute(args); err == nil || !strings.Contains(err.Error(), "already has active skill") {
		t.Fatalf("second active skill was not rejected: %v", err)
	}
	fallbackArgs := map[string]interface{}{
		"reason": "the documented precondition is false", "failed_step_id": "preconditions",
		"error_category": "stale_precondition", "_context": toolCtx, "_invocation_scope": scope,
	}
	if _, err := NewSkillFallbackTool(store).Execute(fallbackArgs); err != nil {
		t.Fatal(err)
	}
	args["name"] = "deploy-other"
	if _, err := NewSkillSelectTool(store).Execute(args); err == nil || !strings.Contains(err.Error(), "ordinary planning") {
		t.Fatalf("replacement after fallback was not rejected: %v", err)
	}
}

func TestSkillLifecycleManagementPromotesRollsBackAndBindsExplicitly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "manager", "Manager")
	task, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "repeatable inspection", Channel: "cli"})
	root, err := userSkillsDirForTenant(identity.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	name := "repeatable-inspection"
	key := control.SkillKey(identity.TenantID, name, SkillScopeUser, SkillSourceAgentCreated, root, filepath.ToSlash(filepath.Join(name, "SKILL.md")))
	contentV1 := curatedSkillTestContent(name, "Read the manifest and verify the answer.")
	versionV1, err := store.CreateSkillCandidateVersion(ctx, identity.TenantID, key, name, "", contentV1, "evidence-v1", []string{"o1", "o2", "o3"}, map[string]string{"risk": "read_only"})
	if err != nil {
		t.Fatal(err)
	}
	tool := NewSkillLifecycleManageTool(store)
	if ToolMetadataFor(tool).Exposure != ToolExposureHidden {
		t.Fatal("lifecycle management tool must remain hidden from model discovery")
	}
	directScope := kernel.ToolInvocationScope{
		ControlTenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID,
		SkillMutationMode: kernel.SkillMutationDirect,
	}
	baseArgs := map[string]interface{}{"_tenant_id": identity.TenantID, "_context": ctx, "_invocation_scope": directScope}
	promote := cloneLifecycleArgs(baseArgs, map[string]interface{}{"action": "candidate_promote", "version_hash": versionV1})
	if _, err := tool.Execute(promote); err != nil {
		t.Fatal(err)
	}
	active, err := store.GetSkillVersion(ctx, identity.TenantID, key, versionV1)
	if err != nil || active == nil || active.State != "active" {
		t.Fatalf("first version=%+v err=%v", active, err)
	}

	contentV2 := curatedSkillTestContent(name, "Read only the declared manifest, omit redundant scans, and verify the answer.")
	versionV2, err := store.CreateSkillCandidateVersion(ctx, identity.TenantID, key, name, versionV1, contentV2, "evidence-v2", []string{"o4", "o5", "o6"}, map[string]string{"risk": "read_only"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(cloneLifecycleArgs(baseArgs, map[string]interface{}{"action": "candidate_promote", "version_hash": versionV2})); err != nil {
		t.Fatal(err)
	}
	previous, _ := store.GetSkillVersion(ctx, identity.TenantID, key, versionV1)
	if previous == nil || previous.State != "previous" {
		t.Fatalf("first version was not retained for rollback: %+v", previous)
	}
	if _, err := tool.Execute(cloneLifecycleArgs(baseArgs, map[string]interface{}{"action": "rollback", "version_hash": versionV1})); err != nil {
		t.Fatal(err)
	}
	rolledBack, _ := store.GetSkillVersion(ctx, identity.TenantID, key, versionV1)
	if rolledBack == nil || rolledBack.State != "active" {
		t.Fatalf("rollback did not reactivate first version: %+v", rolledBack)
	}

	if _, err := tool.Execute(cloneLifecycleArgs(baseArgs, map[string]interface{}{"action": "binding_bind", "name": name})); err != nil {
		t.Fatal(err)
	}
	binding, err := store.GetTaskSkillBinding(ctx, identity.TenantID, identity.PersonID, task.ID)
	if err != nil || binding == nil || binding.SkillKey != key || binding.State != control.TaskSkillBindingActive {
		t.Fatalf("binding=%+v err=%v", binding, err)
	}
	run, err := store.StartRun(ctx, task, "cli", "repeat the bound inspection")
	if err != nil {
		t.Fatal(err)
	}
	selectionScope := directScope
	selectionScope.RunID = run.ID
	selectionScope.WorkUnitID = run.WorkUnitID
	selectionScope.ExecutionLane = "main"
	selected, err := NewSkillSelectTool(store).Execute(map[string]interface{}{
		"reason": "resolve the related task default", "_tenant_id": identity.TenantID,
		"_context": ctx, "_invocation_scope": selectionScope,
	})
	if err != nil || !strings.Contains(selected, `"name":"`+name+`"`) {
		t.Fatalf("work-unit binding resolution=%s err=%v", selected, err)
	}
	if _, err := tool.Execute(cloneLifecycleArgs(baseArgs, map[string]interface{}{"action": "binding_unbind"})); err != nil {
		t.Fatal(err)
	}
	binding, _ = store.GetTaskSkillBinding(ctx, identity.TenantID, identity.PersonID, task.ID)
	if binding == nil || binding.State != control.TaskSkillBindingReleased {
		t.Fatalf("binding was not released: %+v", binding)
	}

	deniedScope := directScope
	deniedScope.SkillMutationMode = kernel.SkillMutationCandidateOnly
	if _, err := tool.Execute(map[string]interface{}{
		"action": "rollback", "version_hash": versionV2, "_tenant_id": identity.TenantID,
		"_invocation_scope": deniedScope,
	}); err == nil {
		t.Fatal("candidate-only authority performed an active rollback")
	}
}

func TestCandidateOnlyAuthorityCreatesCandidateButCannotPublish(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root, _ := userSkillsDirForTenant("default")
	name := "candidate-only"
	key := control.SkillKey("default", name, SkillScopeUser, SkillSourceAgentCreated, root, filepath.ToSlash(filepath.Join(name, "SKILL.md")))
	scope := kernel.ToolInvocationScope{ControlTenantID: "default", SkillMutationMode: kernel.SkillMutationCandidateOnly}
	result, err := NewSkillLifecycleManageTool(store).Execute(map[string]interface{}{
		"action": "candidate_create", "skill_key": key, "name": name,
		"content":           curatedSkillTestContent(name, "Read the declared file."),
		"evidence_set_hash": "candidate-only-evidence", "observation_ids": []string{"o1", "o2", "o3"},
		"evidence_json": `{}`, "_context": ctx, "_invocation_scope": scope,
	})
	if err != nil || !strings.Contains(result, `"state":"candidate"`) {
		t.Fatalf("candidate-only creation=%s err=%v", result, err)
	}
	var version struct {
		VersionHash string `json:"version_hash"`
	}
	_ = json.Unmarshal([]byte(result), &version)
	if _, err := NewSkillLifecycleManageTool(store).Execute(map[string]interface{}{
		"action": "candidate_promote", "skill_key": key, "version_hash": version.VersionHash,
		"_context": ctx, "_invocation_scope": scope,
	}); err == nil || !strings.Contains(err.Error(), "candidate_only") {
		t.Fatalf("candidate-only authority published active content: %v", err)
	}
}

func curatedSkillTestContent(name, procedure string) string {
	return "---\nname: " + name + "\ndescription: Narrow repeatable inspection.\n---\n\n" +
		"## Applicability\nUse only for the matching inspection.\n\n" +
		"## Inputs\nA declared manifest.\n\n## Preconditions\nThe manifest exists.\n\n" +
		"## Procedure\n" + procedure + "\n\n## Failure Guards\nDo not guess missing data.\n\n" +
		"## Recovery\nReturn to ordinary planning.\n\n## Verification\nCite the manifest evidence.\n"
}

func cloneLifecycleArgs(base, extra map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(base)+len(extra))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range extra {
		out[key] = value
	}
	return out
}

func TestSkillViewIsInspectionNotActivation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := NewSkillManageTool().Execute(directSkillMutationArgs(map[string]interface{}{
		"action": "create", "name": "inspect-only", "content": "Inspect this procedure.",
	})); err != nil {
		t.Fatal(err)
	}
	events := make(chan string, 1)
	ctx := kernel.WithEventChannel(context.Background(), events)
	if _, err := NewSkillViewTool().Execute(map[string]interface{}{"name": "inspect-only", "_context": ctx}); err != nil {
		t.Fatal(err)
	}
	event, ok := kernel.DecodeAgentEvent(<-events)
	if !ok || event.Type != "skill.viewed" {
		t.Fatalf("skill_view emitted execution attribution: %+v ok=%v", event, ok)
	}
}
