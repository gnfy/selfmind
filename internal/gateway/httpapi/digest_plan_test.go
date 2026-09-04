package httpapi

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
)

// TestDigestCarriesTheLatestPlanPayload pins the server half of progress
// visibility on re-attach. PlanSteps is pre-rendered text for the one-shot
// digest; a client also needs the STRUCTURED snapshot so its pinned plan view
// comes back through the same renderer live events feed. Without it, attaching
// mid-run said what was running but never how far along.
func TestDigestCarriesTheLatestPlanPayload(t *testing.T) {
	daemon, store, _, task, run := newClarifyTestServer(t)
	ctx := context.Background()

	if payload := daemon.latestPlanPayloadForTask(ctx, task.ID); payload != "" {
		t.Fatalf("a run with no plan should carry none, got %q", payload)
	}

	first := `{"plan":[{"step":"one","status":"in_progress"},{"step":"two","status":"pending"}]}`
	if _, err := store.AppendEvent(ctx, control.Event{
		TaskID: task.ID, RunID: run.ID, Type: "plan.updated", Visibility: "task",
		Channel: "cli", Payload: []byte(first),
	}); err != nil {
		t.Fatal(err)
	}
	newest := `{"plan":[{"step":"one","status":"completed"},{"step":"two","status":"in_progress"}]}`
	if _, err := store.AppendEvent(ctx, control.Event{
		TaskID: task.ID, RunID: run.ID, Type: "plan.updated", Visibility: "task",
		Channel: "cli", Payload: []byte(newest),
	}); err != nil {
		t.Fatal(err)
	}

	payload := daemon.latestPlanPayloadForTask(ctx, task.ID)
	if payload == "" {
		t.Fatal("the digest carried no plan payload for a run that published one")
	}
	// Every update is a complete snapshot, so the NEWEST one is the answer;
	// returning the first would show stale progress after a re-attach.
	if !strings.Contains(payload, `"step":"one","status":"completed"`) {
		t.Fatalf("payload is not the newest snapshot: %s", payload)
	}
}
