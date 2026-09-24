package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/kernel/llm"
)

func TestCompletedRunToolReceiptsAreExactBoundedAndDeduplicated(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "alice", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Inspect", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "Inspect")
	if err != nil {
		t.Fatal(err)
	}
	messages := []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "a", Function: "read_file", Args: `{"path":"plan.json"}`}}},
		{Role: "tool", ToolCallID: "a", Content: "ready=true"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "b", Function: "read_file", Args: `{"path":"plan.json"}`}}},
		{Role: "tool", ToolCallID: "b", Content: "ready=true\nAuthorization: Bearer secret123"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c", Function: "finish_run", Args: `{}`}}},
		{Role: "tool", ToolCallID: "c", Content: "done"},
	}
	snapshot, _ := json.Marshal(messages)
	if err := store.SaveLoopCheckpoint(ctx, control.LoopCheckpointRecord{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID,
		RunID: run.ID, Outcome: "complete_turn", Snapshot: snapshot,
	}); err != nil {
		t.Fatal(err)
	}
	receipts := CompletedRunToolReceipts(ctx, store, identity.TenantID, identity.PersonID, run.ID)
	if len(receipts) != 1 || receipts[0].Tool != "read_file" || receipts[0].Target != "plan.json" ||
		!strings.Contains(receipts[0].Excerpt, "ready=true") || strings.Contains(receipts[0].Excerpt, "secret123") {
		t.Fatalf("receipts=%+v", receipts)
	}
	if got := CompletedRunToolReceipts(ctx, store, identity.TenantID, "another-person", run.ID); len(got) != 0 {
		t.Fatalf("foreign person saw receipts: %+v", got)
	}
	if got := CompletedRunToolReceipts(ctx, store, identity.TenantID, identity.PersonID, "another-run"); len(got) != 0 {
		t.Fatalf("foreign run saw receipts: %+v", got)
	}
	if err := store.SaveLoopCheckpoint(ctx, control.LoopCheckpointRecord{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID,
		RunID: run.ID, Outcome: "continue", Snapshot: snapshot,
	}); err != nil {
		t.Fatal(err)
	}
	if got := CompletedRunToolReceipts(ctx, store, identity.TenantID, identity.PersonID, run.ID); len(got) != 0 {
		t.Fatalf("unfinished checkpoint was treated as a completed prior turn: %+v", got)
	}
}
