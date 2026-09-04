package tools

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/kernel"
)

func TestWorkSelectRecordsTypedPersonScopedProposal(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	person, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "alice", "Alice")
	targetTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, Title: "target", Channel: "cli"})
	targetRun, _ := store.StartRun(ctx, targetTask, "cli", "target work")
	_ = store.FinishRun(ctx, person.TenantID, targetRun.ID, "interrupted")
	correctTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, Title: "correct target", Channel: "cli"})
	correctRun, _ := store.StartRun(ctx, correctTask, "cli", "correct target work")
	_ = store.FinishRun(ctx, person.TenantID, correctRun.ID, "interrupted")
	interactionTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, Title: "progress question", Channel: "weixin"})
	// The interaction runs in another execution domain, so a resume stays a
	// typed proposal for the gateway's transfer commit instead of being
	// claimed in place.
	interactionRun, _ := store.StartRunWithOptions(ctx, interactionTask, "weixin", "how is target going", control.StartRunOptions{
		ExecutionRoots: []executionenv.RootBinding{{Path: "/weixin-scope", Role: executionenv.RootRolePrimary, AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceWorkspace}},
	})

	tool := NewWorkSelectTool(store)
	result, err := tool.Execute(map[string]interface{}{
		"action": "resume", "run_id": targetRun.ID,
		"_invocation_scope": kernel.ToolInvocationScope{
			ControlTenantID: person.TenantID, PersonID: person.PersonID,
			TaskID: interactionTask.ID, RunID: interactionRun.ID, ExecutionLane: "main",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"status":"proposed"`) || !strings.Contains(result, targetRun.ID) {
		t.Fatalf("proposal result = %s", result)
	}
	events, err := store.ListRunEvents(ctx, person.TenantID, person.PersonID, interactionTask.ID, interactionRun.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != "work.selection" || !strings.Contains(string(events[0].Payload), `"action":"resume"`) {
		t.Fatalf("selection events = %+v", events)
	}
	// An identical retry converges. A different selection is an audited
	// correction while this interaction has produced only read-only evidence.
	if _, err := tool.Execute(map[string]interface{}{
		"action": "resume", "run_id": targetRun.ID,
		"_invocation_scope": kernel.ToolInvocationScope{ControlTenantID: person.TenantID, PersonID: person.PersonID, TaskID: interactionTask.ID, RunID: interactionRun.ID},
	}); err != nil {
		t.Fatalf("identical retry: %v", err)
	}
	corrected, err := tool.Execute(map[string]interface{}{
		"action": "resume", "run_id": correctRun.ID,
		"_invocation_scope": kernel.ToolInvocationScope{ControlTenantID: person.TenantID, PersonID: person.PersonID, TaskID: interactionTask.ID, RunID: interactionRun.ID},
	})
	if err != nil || !strings.Contains(corrected, `"status":"corrected"`) {
		t.Fatalf("safe correction = %s err=%v", corrected, err)
	}
	events, _ = store.ListRunEvents(ctx, person.TenantID, person.PersonID, interactionTask.ID, interactionRun.ID, 10)
	if len(events) != 2 || !strings.Contains(string(events[0].Payload), correctRun.ID) ||
		!strings.Contains(string(events[0].Payload), targetRun.ID) {
		t.Fatalf("correction audit = %+v", events)
	}
	claim, _ := store.ClaimToolDispatch(ctx, person.TenantID, control.ToolLedgerEntry{
		RunID: interactionRun.ID, ToolCallID: "write", ToolName: "patch", ArgsHash: "x", RetryClass: "side_effect",
	})
	if !claim.Execute {
		t.Fatal("failed to seed material effect")
	}
	_ = store.RecordToolOutcome(ctx, person.TenantID, interactionRun.ID, "write", true)
	if _, err := tool.Execute(map[string]interface{}{
		"action": "resume", "run_id": targetRun.ID,
		"_invocation_scope": kernel.ToolInvocationScope{ControlTenantID: person.TenantID, PersonID: person.PersonID, TaskID: interactionTask.ID, RunID: interactionRun.ID},
	}); err == nil || !strings.Contains(err.Error(), "material effect") {
		t.Fatalf("post-effect correction err=%v", err)
	}
}

func TestWorkSelectRejectsForeignRun(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	alice, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "alice", "Alice")
	bob, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "bob", "Bob")
	aliceTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: alice.TenantID, PersonID: alice.PersonID, Title: "current", Channel: "cli"})
	aliceRun, _ := store.StartRun(ctx, aliceTask, "cli", "current")
	bobTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: bob.TenantID, PersonID: bob.PersonID, Title: "private", Channel: "cli"})
	bobRun, _ := store.StartRun(ctx, bobTask, "cli", "private")
	tool := NewWorkSelectTool(store)
	if _, err := tool.Execute(map[string]interface{}{
		"action": "observe", "run_id": bobRun.ID,
		"_invocation_scope": kernel.ToolInvocationScope{ControlTenantID: alice.TenantID, PersonID: alice.PersonID, TaskID: aliceTask.ID, RunID: aliceRun.ID},
	}); err == nil {
		t.Fatal("foreign target must fail closed")
	}
}
