package httpapi

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/kernel"
	"selfmind/internal/tools"
)

func TestTaskSkillBindingRequiresDeterministicAttachAndHonorsFailureGuard(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store := controltest.NewStore(t)
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

func TestSkillCandidateCatalogueIssuesDurableWorkUnitRefs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store := controltest.NewStore(t)
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "candidate-selector", "Candidate Selector")
	task, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "inspect", Channel: "cli"})
	run, _ := store.StartRun(ctx, task, "cli", "inspect")
	for _, name := range []string{"inspect-alpha", "inspect-beta"} {
		if _, err := tools.NewSkillManageTool().Execute(map[string]interface{}{
			"action": "create", "name": name, "description": "inspect metadata",
			"content": "## Procedure\nInspect and verify.", "source": tools.SkillSourceAgentCreated,
			"_tenant_id":        identity.TenantID,
			"_invocation_scope": kernel.ToolInvocationScope{ControlTenantID: identity.TenantID, SkillMutationMode: kernel.SkillMutationDirect},
		}); err != nil {
			t.Fatal(err)
		}
	}
	scope := kernel.ToolInvocationScope{ControlTenantID: identity.TenantID, PersonID: identity.PersonID,
		TaskID: task.ID, RunID: run.ID, WorkUnitID: run.WorkUnitID, ExecutionLane: "main"}
	selected := (&Server{Control: store}).coordinator().selectSkillRuntimeContext(
		kernel.WithToolInvocationScope(ctx, scope), identity, task, run,
		taskAttach{reason: taskAttachCurrentPreLabel, preLabel: true}, "inspect metadata")
	if selected.Active != nil || len(selected.Candidates) != 2 {
		t.Fatalf("candidate context = %+v", selected)
	}
	for _, candidate := range selected.Candidates {
		if candidate.CandidateRef == "" {
			t.Fatalf("candidate has no ref: %+v", candidate)
		}
		resolved, err := store.ResolveSkillCandidateRef(ctx, identity.TenantID, identity.PersonID, run.ID, run.WorkUnitID, candidate.CandidateRef)
		if err != nil || resolved == nil || resolved.SkillKey != candidate.Key {
			t.Fatalf("durable ref=%+v err=%v candidate=%+v", resolved, err, candidate)
		}
	}
	units, err := store.SyncRunWorkUnits(ctx, identity.TenantID, run.ID, []control.WorkUnitPlanInput{
		{WorkUnitID: run.WorkUnitID, GoalDigest: "inspect alpha", PlanStatus: "completed"},
		{GoalDigest: "inspect beta", PlanStatus: "in_progress"},
	})
	if err != nil || len(units) != 2 {
		t.Fatalf("new work-unit projection=%+v err=%v", units, err)
	}
	newScope := scope
	newScope.WorkUnitID = units[1].ID
	coordinator := (&Server{Control: store}).coordinator()
	candidates, catalog, report, ok := coordinator.prepareSkillCandidateSnapshot(
		kernel.WithToolInvocationScope(ctx, newScope), identity, run.ID, units[1].ID,
		"inspect metadata", newScope, tools.WithSkillStorage(map[string]interface{}{
			"_tenant_id": identity.TenantID, "_context": ctx, "_invocation_scope": newScope,
		}, nil),
	)
	if !ok || report.Included != 2 || !strings.Contains(catalog, "## Skill Candidates for Current Work Unit") {
		t.Fatalf("new work-unit catalogue ok=%v report=%+v catalog=%q", ok, report, catalog)
	}
	if len(catalog) > kernel.DefaultRuntimeContextBudget().SkillCatalogBytes || report.Tokens > report.TokenBudget {
		t.Fatalf("new work-unit catalogue exceeded budget: %+v", report)
	}
	for _, candidate := range candidates[:report.Included] {
		if !strings.Contains(catalog, candidate.CandidateRef) {
			t.Fatalf("issued ref %q missing from new work-unit catalogue %q", candidate.CandidateRef, catalog)
		}
		resolved, resolveErr := store.ResolveSkillCandidateRef(ctx, identity.TenantID, identity.PersonID, run.ID, units[1].ID, candidate.CandidateRef)
		if resolveErr != nil || resolved == nil || resolved.SkillKey != candidate.Key {
			t.Fatalf("new work-unit ref=%+v err=%v candidate=%+v", resolved, resolveErr, candidate)
		}
	}
}

func TestExplicitSlashSkillActivatesSamePinnedDeliveryBeforeDiscovery(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store := controltest.NewStore(t)
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "slash-selector", "Slash Selector")
	task, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "slash", Channel: "cli"})
	run, _ := store.StartRun(ctx, task, "cli", "slash")
	if _, err := tools.NewSkillManageTool().Execute(map[string]interface{}{
		"action": "create", "name": "slash-flow", "description": "slash flow",
		"content": "## Procedure\nApply exact slash procedure.", "source": tools.SkillSourceAgentCreated,
		"_tenant_id":        identity.TenantID,
		"_invocation_scope": kernel.ToolInvocationScope{ControlTenantID: identity.TenantID, SkillMutationMode: kernel.SkillMutationDirect},
	}); err != nil {
		t.Fatal(err)
	}
	explicit, invocation, _, kind, ok, err := tools.ResolveTypedSkillInvocationForTenant(identity.TenantID, "/slash-flow", "perform it")
	if err != nil || !ok {
		t.Fatalf("slash resolution=%q ok=%v err=%v", invocation, ok, err)
	}
	if kind != "skill" {
		t.Fatalf("slash kind=%q", kind)
	}
	scope := kernel.ToolInvocationScope{ControlTenantID: identity.TenantID, PersonID: identity.PersonID,
		TaskID: task.ID, RunID: run.ID, WorkUnitID: run.WorkUnitID, ExecutionLane: "main"}
	selected := (&Server{Control: store}).coordinator().selectSkillRuntimeContext(
		kernel.WithExplicitSkillInvocation(kernel.WithToolInvocationScope(ctx, scope), explicit), identity, task, run,
		taskAttach{reason: taskAttachCurrentPreLabel, preLabel: true}, invocation)
	if selected.Active == nil || selected.Active.Name != "slash-flow" || len(selected.Candidates) != 0 || !strings.Contains(selected.Active.DeliveredMain, "Apply exact slash procedure") {
		t.Fatalf("explicit selection = %+v", selected)
	}
	activation, err := store.ActiveSkillActivation(ctx, identity.TenantID, run.ID, run.WorkUnitID, "main")
	if err != nil || activation == nil || activation.ActivationSource != "slash" || activation.DeliveredMainHash != selected.Active.DeliveredHash {
		t.Fatalf("slash activation=%+v err=%v", activation, err)
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
