package control

import (
	"context"
	"encoding/json"
	"testing"
)

// TestListRecentErrors: tool-failure events and failed-run-outcome events
// merge into one newest-first list; successful tool events and successful run
// outcomes are excluded.
func TestListRecentErrors(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "tenant", "cli", "local", "Tester")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "search"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "find aion n60")
	if err != nil {
		t.Fatal(err)
	}
	appendEvent := func(typ string, payload map[string]interface{}) {
		raw, _ := json.Marshal(payload)
		if _, err := store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: run.ID, Type: typ, Visibility: "task", Payload: raw}); err != nil {
			t.Fatal(err)
		}
	}

	// Tool failure.
	appendEvent("tool.completed", map[string]interface{}{"tool": "web_search", "error": "backend duckduckgo unavailable (HTTP 202 anti-bot)"})
	// Successful tool event — must NOT appear.
	appendEvent("tool.completed", map[string]interface{}{"tool": "web_search", "result": "3 results via tavily"})
	// Failed run outcome (model 429) — must appear with the summary as message.
	appendEvent("run.finished", map[string]interface{}{"outcome": map[string]interface{}{"status": "failed", "summary": "llm chat: responses API error 429: usage limit reached"}})
	// Successful run outcome — must NOT appear.
	appendEvent("run.finished", map[string]interface{}{"outcome": map[string]interface{}{"status": "completed", "summary": "done"}})

	errs, err := store.ListRecentErrors(ctx, identity.TenantID, identity.PersonID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(errs) != 2 {
		t.Fatalf("expected exactly 2 errors (1 tool + 1 run), got %d: %+v", len(errs), errs)
	}
	var sawTool, sawRun bool
	for _, e := range errs {
		switch e.Kind {
		case "tool":
			sawTool = true
			if e.Source != "web_search" || e.Message == "" {
				t.Errorf("tool error wrong: %+v", e)
			}
		case "run":
			sawRun = true
			if e.Source != "failed" || e.Message == "" {
				t.Errorf("run error must carry status+summary: %+v", e)
			}
		}
	}
	if !sawTool || !sawRun {
		t.Fatalf("expected both kinds: tool=%v run=%v", sawTool, sawRun)
	}

	// Person scoping: another person sees nothing.
	other, _ := store.ResolveOrCreateAccount(ctx, "tenant", "cli", "stranger", "Stranger")
	if got, _ := store.ListRecentErrors(ctx, identity.TenantID, other.PersonID, 10); len(got) != 0 {
		t.Fatalf("errors must be person-scoped, stranger saw %d", len(got))
	}
}
