package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
)

func TestWorkSearchWindowReturnsCoverageAndNeverResumes(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	person, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "window-owner", "Owner")
	task, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, Title: "investigate issue", Channel: "cli"})
	run, _ := store.StartRun(ctx, task, "cli", "Inspect local facts")
	_ = store.FinishRun(ctx, person.TenantID, run.ID, "interrupted")
	args := map[string]interface{}{"since": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), "until": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "_invocation_scope": kernel.ToolInvocationScope{ControlTenantID: person.TenantID, PersonID: person.PersonID, RunID: "current"}}
	out, err := NewWorkSearchTool(store).Execute(args)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Count    int
		HasMore  bool `json:"has_more"`
		Coverage string
	}
	if err = json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if result.Count != 1 || result.HasMore || result.Coverage != "end_of_window" || !strings.Contains(out, run.ID) {
		t.Fatalf("missing bounded coverage: %s", out)
	}
	stored, _ := store.GetRun(ctx, person.TenantID, run.ID)
	if stored.Status != "interrupted" {
		t.Fatal("history query resumed work")
	}
	args["since"] = "yesterday"
	if _, err = NewWorkSearchTool(store).Execute(args); err == nil {
		t.Fatal("natural language time silently selected a window")
	}
}
