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

func TestCompletedRunToolReceiptsRetainSkillViewAcrossLaterReads(t *testing.T) {
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
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Skill", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "Skill")
	if err != nil {
		t.Fatal(err)
	}
	messages := []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "s1", Function: "skill_view", Args: `{"name":"cicd-deploy"}`}}},
		{Role: "tool", ToolCallID: "s1", Content: `{"content":"# CI/CD Deploy Skill\nStep 1: Verify cluster"}`},
		{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "r1", Function: "read_file", Args: `{"path":"f1.txt"}`},
			{ID: "r2", Function: "read_file", Args: `{"path":"f2.txt"}`},
			{ID: "r3", Function: "read_file", Args: `{"path":"f3.txt"}`},
			{ID: "r4", Function: "read_file", Args: `{"path":"f4.txt"}`},
			{ID: "r5", Function: "read_file", Args: `{"path":"f5.txt"}`},
			{ID: "r6", Function: "read_file", Args: `{"path":"f6.txt"}`},
		}},
		{Role: "tool", ToolCallID: "r1", Content: "c1"},
		{Role: "tool", ToolCallID: "r2", Content: "c2"},
		{Role: "tool", ToolCallID: "r3", Content: "c3"},
		{Role: "tool", ToolCallID: "r4", Content: "c4"},
		{Role: "tool", ToolCallID: "r5", Content: "c5"},
		{Role: "tool", ToolCallID: "r6", Content: "c6"},
	}
	snapshot, _ := json.Marshal(messages)
	if err := store.SaveLoopCheckpoint(ctx, control.LoopCheckpointRecord{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID,
		RunID: run.ID, Outcome: "complete_turn", Snapshot: snapshot,
	}); err != nil {
		t.Fatal(err)
	}
	receipts := CompletedRunToolReceipts(ctx, store, identity.TenantID, identity.PersonID, run.ID)
	if len(receipts) != 6 {
		t.Fatalf("len(receipts) = %d, want 6", len(receipts))
	}
	foundSkillView := false
	for _, r := range receipts {
		if r.Tool == "skill_view" && r.Target == "cicd-deploy" && strings.Contains(r.Excerpt, "CI/CD Deploy Skill") {
			foundSkillView = true
		}
	}
	if !foundSkillView {
		t.Fatalf("expected skill_view receipt to be preserved across 6 later read_file calls: %+v", receipts)
	}

	// Also verify when 4 large (>= 900 byte) read_file outputs exhaust the 3600-byte cap before 6 items.
	bigContent := strings.Repeat("x", 1000)
	bigMessages := []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "s1", Function: "skill_view", Args: `{"name":"cicd-deploy"}`}}},
		{Role: "tool", ToolCallID: "s1", Content: `{"content":"# CI/CD Deploy Skill\nStep 1: Verify cluster"}`},
		{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "r1", Function: "read_file", Args: `{"path":"f1.txt"}`},
			{ID: "r2", Function: "read_file", Args: `{"path":"f2.txt"}`},
			{ID: "r3", Function: "read_file", Args: `{"path":"f3.txt"}`},
			{ID: "r4", Function: "read_file", Args: `{"path":"f4.txt"}`},
		}},
		{Role: "tool", ToolCallID: "r1", Content: bigContent},
		{Role: "tool", ToolCallID: "r2", Content: bigContent},
		{Role: "tool", ToolCallID: "r3", Content: bigContent},
		{Role: "tool", ToolCallID: "r4", Content: bigContent},
	}
	bigSnapshot, _ := json.Marshal(bigMessages)
	if err := store.SaveLoopCheckpoint(ctx, control.LoopCheckpointRecord{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID,
		RunID: run.ID, Outcome: "complete_turn", Snapshot: bigSnapshot,
	}); err != nil {
		t.Fatal(err)
	}
	bigReceipts := CompletedRunToolReceipts(ctx, store, identity.TenantID, identity.PersonID, run.ID)
	foundSkillViewBig := false
	for _, r := range bigReceipts {
		if r.Tool == "skill_view" && r.Target == "cicd-deploy" {
			foundSkillViewBig = true
		}
	}
	if !foundSkillViewBig {
		t.Fatalf("expected skill_view receipt to be preserved when 4 large read_file outputs exhaust byte budget: %+v", bigReceipts)
	}
}
