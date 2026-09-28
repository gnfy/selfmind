package tools

import (
	"context"
	"errors"
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

// Resuming a run that has nothing left to resume is refused with what to do
// instead. qwen twice tried to resume a finished run named in its work
// history, retried, and reported the bare refusal to the person.
func TestWorkSelectSaysWhatToDoInsteadOfResumingSettledWork(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	person, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "alice", "Alice")
	finishedTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, Title: "release notes", Channel: "cli"})
	finished, _ := store.StartRun(ctx, finishedTask, "cli", "write the release notes")
	_ = store.FinishRun(ctx, person.TenantID, finished.ID, "done")
	lineTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, Title: "migration", Channel: "cli"})
	older, _ := store.StartRun(ctx, lineTask, "cli", "start the migration")
	_ = store.FinishRun(ctx, person.TenantID, older.ID, "done")
	latest, _ := store.StartRun(ctx, lineTask, "cli", "continue the migration")
	_ = store.FinishRun(ctx, person.TenantID, latest.ID, "interrupted")
	currentTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, Title: "continue", Channel: "cli"})
	current, _ := store.StartRun(ctx, currentTask, "cli", "继续")
	scope := kernel.ToolInvocationScope{ControlTenantID: person.TenantID, PersonID: person.PersonID, TaskID: currentTask.ID, RunID: current.ID}

	tool := NewWorkSelectTool(store)
	for _, tc := range []struct {
		target string
		want   []string
	}{
		{finished.ID, []string{"is done; it has nothing left to resume", "Do not retry resume", "in the current run"}},
		{older.ID, []string{"the resumable run of the same work is " + latest.ID, "Resume " + latest.ID + " instead"}},
	} {
		_, err := tool.Execute(map[string]interface{}{"action": "resume", "run_id": tc.target, "_invocation_scope": scope})
		var refusal interface {
			ToolErrorCode() string
			ModelSafeMessage() string
			ToolRecoveryHint() string
		}
		if !errors.As(err, &refusal) || refusal.ToolErrorCode() != "work_run_not_resumable" {
			t.Fatalf("resume %s: err=%v, want a typed not-resumable refusal", tc.target, err)
		}
		text := refusal.ModelSafeMessage() + " " + refusal.ToolRecoveryHint()
		for _, want := range tc.want {
			if !strings.Contains(text, want) {
				t.Errorf("resume %s: refusal %q, want %q", tc.target, text, want)
			}
		}
	}
}
