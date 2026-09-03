package tools

import (
	"context"
	"encoding/json"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
)

// Ordinary history search must not inject unrelated Attention. Main can ask
// for mode=attention when a short confirmation has no useful lexical terms.
func TestWorkSearchSeparatesHistoryQueryFromAttention(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	parked, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "aws 生产发布 cw2-seoant-ai-prod-api", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := store.StartRun(ctx, parked, "cli", "aws生产发布，先预检再等我确认")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, waiting.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	interaction, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "确认执行", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.StartRun(ctx, interaction, "cli", "确认执行")
	if err != nil {
		t.Fatal(err)
	}

	tool := NewWorkSearchTool(store)
	args := map[string]interface{}{
		"query": "completely unrelated topic",
		"_invocation_scope": kernel.ToolInvocationScope{
			ControlTenantID: identity.TenantID, PersonID: identity.PersonID,
			TaskID: interaction.ID, RunID: current.ID, ExecutionLane: "main",
		},
	}
	result, err := tool.Execute(args)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Count   int            `json:"count"`
		Results []workTaskCard `json:"results"`
	}
	if err := json.Unmarshal([]byte(result), &decoded); err != nil {
		t.Fatalf("decode result: %v\n%s", err, result)
	}
	if decoded.Count != 0 || len(decoded.Results) != 0 {
		t.Fatalf("ordinary query leaked unrelated attention: %s", result)
	}
	args["mode"] = "attention"
	result, err = tool.Execute(args)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(result), &decoded); err != nil {
		t.Fatalf("decode attention result: %v\n%s", err, result)
	}
	if decoded.Count != 1 || len(decoded.Results) != 1 {
		t.Fatalf("expected exactly the parked attention item, got %s", result)
	}
	card := decoded.Results[0]
	if card.TaskID != parked.ID || len(card.Evidence) != 1 || card.Evidence[0] != "unresolved_run" {
		t.Fatalf("parked run must surface as unresolved_run evidence: %+v", card)
	}
	if len(card.Runs) != 1 || card.Runs[0].RunID != waiting.ID || card.Runs[0].Status != "waiting_user" {
		t.Fatalf("card must carry the exact waiting run: %+v", card.Runs)
	}
}

func TestWorkSearchAttentionReturnsOneExactRunPerCard(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	// Two releases parked in two threads: each is the latest run of its own
	// thread, so each is one exact resumable card. (Within one thread only the
	// latest run stays resumable; an older parked run is superseded.)
	inputs := []string{"confirm api", "confirm worker"}
	want := map[string]string{}
	for _, input := range inputs {
		task, err := store.CreateTask(ctx, control.TaskCreate{
			TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "pending release: " + input,
		})
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.StartRun(ctx, task, "cli", input)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_user"); err != nil {
			t.Fatal(err)
		}
		want[run.ID] = input
	}
	current, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "current query",
	})
	if err != nil {
		t.Fatal(err)
	}
	currentRun, err := store.StartRun(ctx, current, "cli", "what needs attention")
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewWorkSearchTool(store).Execute(map[string]interface{}{
		"mode": "attention", "limit": 8,
		"_invocation_scope": kernel.ToolInvocationScope{
			ControlTenantID: identity.TenantID, PersonID: identity.PersonID,
			TaskID: current.ID, RunID: currentRun.ID, ExecutionLane: "main",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Results []workTaskCard `json:"results"`
	}
	if err := json.Unmarshal([]byte(result), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Results) != 2 {
		t.Fatalf("results=%s", result)
	}
	seen := map[string]bool{}
	for _, card := range decoded.Results {
		if len(card.Runs) != 1 {
			t.Fatalf("attention card mixed runs: %+v", card)
		}
		runID := card.Runs[0].RunID
		if _, ok := want[runID]; !ok || card.Summary != want[runID] {
			t.Fatalf("card=%+v want=%+v", card, want)
		}
		seen[runID] = true
	}
	if len(seen) != 2 {
		t.Fatalf("duplicate exact run cards: %+v", decoded.Results)
	}
}
