package httpapi

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/tools"
)

func TestTaskSkillBindingRequiresDeterministicAttachAndHonorsFailureGuard(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "selector", "Selector")
	task, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "inspect build", Channel: "cli"})
	if _, err := tools.NewSkillManageTool().Execute(map[string]interface{}{
		"action": "create", "name": "inspect-build", "content": "Inspect the declared build metadata and verify it.",
		"source": tools.SkillSourceAgentCreated, "_tenant_id": identity.TenantID,
		"_invocation_scope": kernel.ToolInvocationScope{
			ControlTenantID: identity.TenantID, SkillMutationMode: kernel.SkillMutationDirect,
		},
	}); err != nil {
		t.Fatal(err)
	}
	info, content, _, err := tools.ReadSkillPayloadForTenant(identity.TenantID, "inspect-build", "")
	if err != nil {
		t.Fatal(err)
	}
	rel, _ := filepath.Rel(info.Root, skillInfoMainPath(info))
	key := control.SkillKey(identity.TenantID, info.Name, info.Scope, info.Source, info.Root, rel)
	digest := sha256.Sum256([]byte(content))
	version := fmt.Sprintf("%x", digest[:])
	if _, err := store.BindTaskSkill(ctx, control.BindTaskSkillInput{
		IdentityTenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID,
		ControlTenantID: identity.TenantID, SkillKey: key, SkillName: info.Name,
		BindingSource: "explicit", VersionHash: version,
	}); err != nil {
		t.Fatal(err)
	}
	run, _ := store.StartRun(ctx, task, "cli", "inspect build 12345 at /tmp/build-123")
	scope := kernel.ToolInvocationScope{
		ControlTenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID,
		RunID: run.ID, WorkUnitID: run.WorkUnitID, ExecutionLane: "main", AttachmentMode: string(taskAttachExplicitTaskID),
	}
	runCtx := kernel.WithToolInvocationScope(ctx, scope)
	coordinator := (&Server{Control: store}).coordinator()
	weak := coordinator.selectSkillRuntimeContext(runCtx, identity, task, run, taskAttach{reason: taskAttachCurrentPreLabel, preLabel: true}, "inspect build")
	if weak.Active != nil {
		t.Fatal("weak pre-label loaded a durable task Skill binding")
	}
	explicit := coordinator.selectSkillRuntimeContext(runCtx, identity, task, run, taskAttach{reason: taskAttachExplicitTaskID}, "inspect build")
	if explicit.Active == nil || explicit.Active.Key != key || len(explicit.Candidates) != 0 {
		t.Fatalf("deterministic attach did not load exactly one bound Skill: %+v", explicit)
	}
	if _, err := store.FallbackCurrentSkill(ctx, control.SkillFallbackInput{
		IdentityTenantID: identity.TenantID, RunID: run.ID, WorkUnitID: run.WorkUnitID,
		Reason: "known stale step", FailureSignature: "known-stale", FailedStepID: "inspect",
		ErrorCategory: "stale_precondition",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MaterializeRunFinalization(ctx, control.RunFinalization{
		Identity: *identity, RunID: run.ID, RunStatus: "done", TaskID: task.ID,
		TaskStatus: "in_progress", Summary: "recovered without the Skill", VerificationState: "passed",
		Channel: "cli", Event: control.Event{Type: "run.finished", Payload: []byte(`{"status":"done"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	run2, _ := store.StartRun(ctx, task, "cli", "inspect build 67890 at /tmp/build-456")
	scope.RunID, scope.WorkUnitID = run2.ID, run2.WorkUnitID
	guarded := coordinator.selectSkillRuntimeContext(kernel.WithToolInvocationScope(ctx, scope), identity, task, run2, taskAttach{reason: taskAttachContinuation}, "inspect build")
	if guarded.Active != nil || len(guarded.Candidates) != 0 {
		t.Fatalf("known failure guard did not return directly to ordinary planning: %+v", guarded)
	}
	binding, err := store.GetTaskSkillBinding(ctx, identity.TenantID, identity.PersonID, task.ID)
	if err != nil || binding == nil || binding.State != control.TaskSkillBindingSuspended {
		t.Fatalf("repeated known guard did not suspend the binding: %+v err=%v", binding, err)
	}
}

func TestProjectWorkUnitPlanGroupsRefinementsAndKeepsIndependentBoundaries(t *testing.T) {
	plan := projectWorkUnitPlan([]workUnitProjectionStep{
		{Step: "inspect files", Status: "completed"},
		{Step: "edit implementation", Status: "in_progress"},
		{Step: "run tests", Status: "pending"},
	})
	if len(plan) != 1 || plan[0].PlanStatus != "in_progress" {
		t.Fatalf("ordinary refinements became separate work units: %+v", plan)
	}
	plan = projectWorkUnitPlan([]workUnitProjectionStep{
		{Step: "finish task A", Status: "completed", WorkUnitID: "wu-a"},
		{Step: "inspect task B", Status: "in_progress", WorkUnit: true, RelatedTaskID: "task-b", WorkUnitID: "wu-b"},
		{Step: "verify task B", Status: "pending"},
	})
	if len(plan) != 2 || plan[0].WorkUnitID != "wu-a" || plan[1].WorkUnitID != "wu-b" || plan[1].RelatedTaskID != "task-b" {
		t.Fatalf("independent work-unit boundaries were not preserved: %+v", plan)
	}
}
