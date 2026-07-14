package control

import (
	"context"
	"testing"
	"time"
)

func TestPersonEventCursorIsDurableScopedAndNeverReused(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	a, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "alice", "Alice")
	if err != nil {
		t.Fatalf("identity alice: %v", err)
	}
	b, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "bob", "Bob")
	if err != nil {
		t.Fatalf("identity bob: %v", err)
	}
	taskA, err := store.CreateTask(ctx, TaskCreate{TenantID: a.TenantID, PersonID: a.PersonID, Title: "A", Channel: "cli"})
	if err != nil {
		t.Fatalf("task A: %v", err)
	}
	taskB, err := store.CreateTask(ctx, TaskCreate{TenantID: b.TenantID, PersonID: b.PersonID, Title: "B", Channel: "cli"})
	if err != nil {
		t.Fatalf("task B: %v", err)
	}

	committed := make(chan Event, 3)
	unsubscribe := store.SubscribeEventAppends(func(event Event) { committed <- event })
	defer unsubscribe()
	e1, err := store.AppendEvent(ctx, Event{TaskID: taskA.ID, Type: "tool.started"})
	if err != nil {
		t.Fatalf("append A1: %v", err)
	}
	e2, err := store.AppendEvent(ctx, Event{TaskID: taskB.ID, Type: "tool.started"})
	if err != nil {
		t.Fatalf("append B1: %v", err)
	}
	if e2.Cursor <= e1.Cursor {
		t.Fatalf("cursors not increasing: %d then %d", e1.Cursor, e2.Cursor)
	}
	select {
	case got := <-committed:
		if got.Cursor != e1.Cursor || got.PersonID != a.PersonID {
			t.Fatalf("committed event=%+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("committed append was not published")
	}

	replayed, err := store.ListPersonEventsAfter(ctx, a.TenantID, a.PersonID, 0, 20)
	if err != nil {
		t.Fatalf("replay A: %v", err)
	}
	if len(replayed) != 1 || replayed[0].Cursor != e1.Cursor || replayed[0].PersonID != a.PersonID {
		t.Fatalf("person-scoped replay=%+v", replayed)
	}

	if _, err := store.db.ExecContext(ctx, `DELETE FROM task_events WHERE id = ?`, e2.ID); err != nil {
		t.Fatalf("delete highest cursor: %v", err)
	}
	e3, err := store.AppendEvent(ctx, Event{TaskID: taskA.ID, Type: "tool.completed"})
	if err != nil {
		t.Fatalf("append A2: %v", err)
	}
	if e3.Cursor <= e2.Cursor {
		t.Fatalf("cursor was reused after cleanup: deleted=%d next=%d", e2.Cursor, e3.Cursor)
	}
	latest, err := store.LatestPersonEventCursor(ctx, a.TenantID, a.PersonID)
	if err != nil || latest != e3.Cursor {
		t.Fatalf("latest cursor=%d err=%v, want %d", latest, err, e3.Cursor)
	}
}
