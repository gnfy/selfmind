package tools

import (
	"context"
	"encoding/json"
	"os"
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
	deploySafeRef := issueSkillCandidateRefForTest(t, store, identity, run, "deploy-safe")
	deployOtherRef := issueSkillCandidateRefForTest(t, store, identity, run, "deploy-other")
	args := map[string]interface{}{
		"candidate_ref": deploySafeRef, "reason": "matches deployment work",
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

	args["candidate_ref"] = deployOtherRef
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
	args["candidate_ref"] = deployOtherRef
	if _, err := NewSkillSelectTool(store).Execute(args); err == nil || !strings.Contains(err.Error(), "ordinary planning") {
		t.Fatalf("replacement after fallback was not rejected: %v", err)
	}
}

func TestSkillCandidateRootPrecedenceDriftReturnsRefreshableStale(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "candidate-root-drift", "Candidate Root Drift")
	task, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "root drift", Channel: "cli"})
	run, _ := store.StartRun(ctx, task, "cli", "root drift")
	workspace := t.TempDir()
	cleanup := SetExecutionScope(identity.PersonID, ExecutionScope{
		TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: run.ID,
		WorkspaceRoot: workspace, AllowedRoots: []string{workspace},
	})
	defer cleanup()
	if _, err := NewSkillManageTool().Execute(directSkillMutationArgs(map[string]interface{}{
		"action": "create", "name": "precedence-flow", "description": "stable routing description",
		"content": "## Procedure\nUse the control Skill.", "source": SkillSourceAgentCreated,
	})); err != nil {
		t.Fatal(err)
	}
	ref := issueSkillCandidateRefForTest(t, store, identity, run, "precedence-flow")
	workspaceSkill := filepath.Join(workspace, ".selfmind", "skills", "precedence-flow")
	if err := os.MkdirAll(workspaceSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceSkill, "SKILL.md"), []byte("---\nname: precedence-flow\ndescription: stable routing description\n---\n\n## Procedure\nUse the workspace Skill.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scope := kernel.ToolInvocationScope{ControlTenantID: identity.TenantID, PersonID: identity.PersonID,
		RunID: run.ID, WorkUnitID: run.WorkUnitID, ExecutionLane: "main"}
	selectionCtx := WithExecutionScopeKey(ctx, ExecutionScopeKeyForRun(run.ID))
	_, err = NewSkillSelectTool(store).Execute(map[string]interface{}{
		"candidate_ref": ref, "reason": "old root", "_context": selectionCtx, "_invocation_scope": scope,
	})
	stable, ok := err.(interface {
		ToolErrorCode() string
		ToolRecoveryHint() string
	})
	if !ok || stable.ToolErrorCode() != "candidate_stale" || !strings.Contains(stable.ToolRecoveryHint(), "skills_list") {
		t.Fatalf("root precedence drift error=%T %v", err, err)
	}
}

func TestSkillSelectReturnsCurrentCandidatesForStaleName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "stale-skill", "Stale Skill")
	task, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "deploy", Channel: "cli"})
	run, _ := store.StartRun(ctx, task, "cli", "deploy safely")
	if _, err := NewSkillManageTool().Execute(directSkillMutationArgs(map[string]interface{}{
		"action": "create", "name": "deploy-current", "description": "deploy safely with verification",
		"content": "Check preconditions, deploy, then verify.", "source": SkillSourceAgentCreated,
	})); err != nil {
		t.Fatal(err)
	}
	scope := kernel.ToolInvocationScope{
		ControlTenantID: identity.TenantID, PersonID: identity.PersonID, RunID: run.ID,
		WorkUnitID: run.WorkUnitID, ExecutionLane: "main", AttachmentMode: "continuation",
	}
	listed, err := NewSkillsListTool(store).Execute(map[string]interface{}{
		"query": "deploy safely with verification", "_context": ctx, "_invocation_scope": scope,
	})
	if err != nil || !strings.Contains(listed, `"candidate_ref":`) || !strings.Contains(listed, "deploy-current") {
		t.Fatalf("refreshed candidate list=%s err=%v", listed, err)
	}
	_, err = NewSkillSelectTool(store).Execute(map[string]interface{}{
		"candidate_ref": "skref_missing", "reason": "deploy safely with verification",
		"_context": ctx, "_invocation_scope": scope,
	})
	if err == nil {
		t.Fatal("expected stale Skill error")
	}
	stable, ok := err.(interface {
		ToolErrorCode() string
		ModelSafeMessage() string
		ToolRecoveryHint() string
	})
	if !ok || stable.ToolErrorCode() != "candidate_unknown" || !strings.Contains(stable.ToolRecoveryHint(), "skills_list") {
		t.Fatalf("stale Skill recovery = %T %v", err, err)
	}
}

func TestActiveSkillViewReadsPinnedPackageResourceAfterFilesystemDrift(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "package-view", "Package View")
	task, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "package", Channel: "cli"})
	run, _ := store.StartRun(ctx, task, "cli", "use package")
	if _, err := NewSkillManageTool().Execute(directSkillMutationArgs(map[string]interface{}{
		"action": "create", "name": "package-view", "description": "package view",
		"content": "## Procedure\nRead the linked detail.", "source": SkillSourceAgentCreated,
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := writeSkillSupportFile(identity.TenantID, "package-view", "references/detail.md", "pinned-before"); err != nil {
		t.Fatal(err)
	}
	scope := kernel.ToolInvocationScope{ControlTenantID: identity.TenantID, PersonID: identity.PersonID,
		RunID: run.ID, WorkUnitID: run.WorkUnitID, ExecutionLane: "main"}
	ref := issueSkillCandidateRefForTest(t, store, identity, run, "package-view")
	if _, err := NewSkillSelectTool(store).Execute(map[string]interface{}{
		"candidate_ref": ref, "reason": "matches", "_context": ctx, "_invocation_scope": scope,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := writeSkillSupportFile(identity.TenantID, "package-view", "references/detail.md", "filesystem-after"); err != nil {
		t.Fatal(err)
	}
	if _, err := writeSkillSupportFile(identity.TenantID, "package-view", "references/new-after.md", "new-after"); err != nil {
		t.Fatal(err)
	}
	retried, err := NewSkillSelectTool(store).Execute(map[string]interface{}{
		"candidate_ref": ref, "reason": "idempotent retry", "_context": ctx, "_invocation_scope": scope,
	})
	if err != nil {
		t.Fatal(err)
	}
	var retriedResult struct {
		LinkedFiles []string `json:"linked_files"`
	}
	if err := json.Unmarshal([]byte(retried), &retriedResult); err != nil {
		t.Fatal(err)
	}
	if len(retriedResult.LinkedFiles) != 1 || retriedResult.LinkedFiles[0] != "references/detail.md" {
		t.Fatalf("idempotent selection mixed the drifted manifest into the active package: %s", retried)
	}
	if !strings.Contains(retried, "already has an immutable Skill activation") {
		t.Fatalf("idempotent selection did not explain pinned-package reuse: %s", retried)
	}
	viewed, err := NewSkillViewTool(store).Execute(map[string]interface{}{
		"name": "package-view", "file_path": "references/detail.md", "_context": ctx, "_invocation_scope": scope,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(viewed, "pinned-before") || strings.Contains(viewed, "filesystem-after") {
		t.Fatalf("active package resource drifted: %s", viewed)
	}
}

func TestSkillCandidateAllowsOnePackageDriftThenFailsClosed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "candidate-drift", "Candidate Drift")
	task, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "drift", Channel: "cli"})
	run, _ := store.StartRun(ctx, task, "cli", "drift")
	if _, err := NewSkillManageTool().Execute(directSkillMutationArgs(map[string]interface{}{
		"action": "create", "name": "drift-flow", "description": "stable routing description",
		"content": "## Procedure\nversion one", "source": SkillSourceAgentCreated,
	})); err != nil {
		t.Fatal(err)
	}
	ref := issueSkillCandidateRefForTest(t, store, identity, run, "drift-flow")
	info, source, _, err := ReadSkillPayloadForTenant(identity.TenantID, "drift-flow", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := editSkill(identity.TenantID, info.Name, strings.Replace(source, "version one", "version two", 1), ""); err != nil {
		t.Fatal(err)
	}
	scope := kernel.ToolInvocationScope{ControlTenantID: identity.TenantID, PersonID: identity.PersonID,
		RunID: run.ID, WorkUnitID: run.WorkUnitID, ExecutionLane: "main"}
	selected, err := NewSkillSelectTool(store).Execute(map[string]interface{}{
		"candidate_ref": ref, "reason": "description still matches", "_context": ctx, "_invocation_scope": scope,
	})
	if err != nil || !strings.Contains(selected, `"candidate_notice"`) || !strings.Contains(selected, `"selected_version_hash"`) {
		t.Fatalf("first drift selection=%s err=%v", selected, err)
	}
	_, source, _, err = ReadSkillPayloadForTenant(identity.TenantID, "drift-flow", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := editSkill(identity.TenantID, info.Name, strings.Replace(source, "version two", "version three", 1), ""); err != nil {
		t.Fatal(err)
	}
	_, err = NewSkillSelectTool(store).Execute(map[string]interface{}{
		"candidate_ref": ref, "reason": "old ref", "_context": ctx, "_invocation_scope": scope,
	})
	stable, ok := err.(interface{ ToolErrorCode() string })
	if !ok || stable.ToolErrorCode() != "candidate_stale" {
		t.Fatalf("second drift error=%T %v", err, err)
	}
}

func issueSkillCandidateRefForTest(t *testing.T, store *control.Store, identity *control.IdentityContext, run *control.Run, name string) string {
	t.Helper()
	pack, err := ReadSkillPackageForTenant(identity.TenantID, name)
	if err != nil {
		t.Fatal(err)
	}
	key, err := resolvedSkillKey(identity.TenantID, pack.Info)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := store.IssueSkillCandidateRef(context.Background(), control.IssueSkillCandidateRefInput{
		IdentityTenantID: identity.TenantID, ControlTenantID: identity.TenantID,
		PersonID: identity.PersonID, RunID: run.ID, WorkUnitID: run.WorkUnitID,
		SkillKey: key, SkillName: pack.Info.Name, VersionHash: pack.VersionHash,
		PackageHash: pack.PackageHash, DescriptionHash: pack.DescriptionHash,
	})
	if err != nil {
		t.Fatal(err)
	}
	return issued.CandidateRef
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
	contentV3 := curatedSkillTestContent(name, "Read the declared manifest once, verify the answer, and report the bounded result.")
	versionV3, err := store.CreateSkillCandidateVersion(ctx, identity.TenantID, key, name, versionV2, contentV3, "evidence-v3", []string{"o7", "o8", "o9"}, map[string]string{"risk": "read_only"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(cloneLifecycleArgs(baseArgs, map[string]interface{}{"action": "candidate_promote", "version_hash": versionV3})); err != nil {
		t.Fatal(err)
	}
	previous, _ := store.GetSkillVersion(ctx, identity.TenantID, key, versionV1)
	if previous == nil || previous.State != "previous" {
		t.Fatalf("first version was not retained for rollback: %+v", previous)
	}
	if _, err := tool.Execute(cloneLifecycleArgs(baseArgs, map[string]interface{}{"action": "rollback", "version_hash": versionV2})); err != nil {
		t.Fatal(err)
	}
	rolledBack, _ := store.GetSkillVersion(ctx, identity.TenantID, key, versionV2)
	if rolledBack == nil || rolledBack.State != "active" {
		t.Fatalf("three-version rollback did not reactivate second version: %+v", rolledBack)
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

func TestSkillCandidatePromotionRefusesActiveContentDrift(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	name := "drift-safe-repair"
	root, err := userSkillsDirForTenant("default")
	if err != nil {
		t.Fatal(err)
	}
	key := control.SkillKey("default", name, SkillScopeUser, SkillSourceAgentCreated, root, filepath.ToSlash(filepath.Join(name, "SKILL.md")))
	contentV1 := curatedSkillTestContent(name, "Read the original manifest.")
	versionV1, err := store.CreateSkillCandidateVersion(ctx, "default", key, name, "", contentV1, "drift-v1", []string{"o1"}, map[string]string{"kind": "initial"})
	if err != nil {
		t.Fatal(err)
	}
	tool := NewSkillLifecycleManageTool(store)
	baseArgs := map[string]interface{}{
		"_tenant_id": "default", "_context": ctx,
		"_invocation_scope": kernel.ToolInvocationScope{ControlTenantID: "default", SkillMutationMode: kernel.SkillMutationDirect},
	}
	if _, err := tool.Execute(cloneLifecycleArgs(baseArgs, map[string]interface{}{"action": "candidate_promote", "version_hash": versionV1})); err != nil {
		t.Fatal(err)
	}
	contentV2 := curatedSkillTestContent(name, "Read the current manifest and verify it.")
	versionV2, err := store.CreateSkillCandidateVersion(ctx, "default", key, name, versionV1, contentV2, "drift-v2", []string{"o2"}, map[string]string{"kind": "repair"})
	if err != nil {
		t.Fatal(err)
	}
	drifted := curatedSkillTestContent(name, "A person changed this procedure after candidate creation.")
	if _, err := editSkill("default", name, drifted, "", baseArgs); err != nil {
		t.Fatal(err)
	}
	_, err = tool.Execute(cloneLifecycleArgs(baseArgs, map[string]interface{}{"action": "candidate_promote", "version_hash": versionV2}))
	if err == nil || !strings.Contains(err.Error(), "changed after candidate creation") {
		t.Fatalf("stale candidate promotion error=%v", err)
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
