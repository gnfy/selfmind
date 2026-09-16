package httpapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
)

func TestStatusUsesCurrentRunInsteadOfPreviousWaitHandoff(t *testing.T) {
	d, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	parent, err := store.StartRun(ctx, task, "cli", "wait for prerequisite")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveHandoff(ctx, control.Handoff{ID: "handoff_run_" + parent.ID, TaskID: task.ID, Summary: "old watcher summary", DoneItems: []string{"Registered old watcher"}, NextSteps: []string{"Old watcher will notify you"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, parent.ID, "waiting_external"); err != nil {
		t.Fatal(err)
	}
	child, err := store.StartRun(ctx, task, "cli", "verify deliverable")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(ctx, control.Event{TaskID: task.ID, RunID: child.ID, Type: "tool.started", Payload: mustJSON(map[string]string{"tool": "verify"})}); err != nil {
		t.Fatal(err)
	}
	d.coordinator().beginActive(identity.PersonID, &activeRun{TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: child.ID, StartedAt: time.Now()})
	defer d.coordinator().endActive(identity.PersonID)
	card, err := d.statusReply(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(card, "old watcher") || strings.Contains(card, "Old watcher") || !strings.Contains(card, "Running tool: verify") {
		t.Fatalf("stale or missing current activity: %s", card)
	}
}
