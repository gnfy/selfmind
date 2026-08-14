package control

import (
	"context"
	"testing"
	"time"
)

func TestListPersonEventsSinceIsTimeBoundAndPersonScoped(t *testing.T) {
	ctx := context.Background()
	store, identity, task, _ := newRecoveryFixture(t)
	other, err := store.ResolveOrCreateAccount(ctx, identity.TenantID, "cli", "other-person", "Other")
	if err != nil {
		t.Fatal(err)
	}
	otherTask, err := store.CreateTask(ctx, TaskCreate{TenantID: other.TenantID, PersonID: other.PersonID, Title: "other", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(ctx, Event{TaskID: task.ID, Type: "context.recall", Visibility: "task"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(ctx, Event{TaskID: otherTask.ID, Type: "context.recall", Visibility: "task"}); err != nil {
		t.Fatal(err)
	}

	events, err := store.ListPersonEventsSince(ctx, identity.TenantID, identity.PersonID, time.Now().Add(-time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].TaskID != task.ID {
		t.Fatalf("events=%+v", events)
	}
}

func TestListPersonEventsSinceKeepsNewestRowsInTimelineOrder(t *testing.T) {
	ctx := context.Background()
	store, identity, task, _ := newRecoveryFixture(t)
	for _, eventType := range []string{"oldest", "middle", "newest"} {
		if _, err := store.AppendEvent(ctx, Event{TaskID: task.ID, Type: eventType, Visibility: "task"}); err != nil {
			t.Fatal(err)
		}
	}
	events, err := store.ListPersonEventsSince(ctx, identity.TenantID, identity.PersonID, time.Now().Add(-time.Minute), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Type != "middle" || events[1].Type != "newest" {
		t.Fatalf("events=%+v, want middle then newest", events)
	}
}
