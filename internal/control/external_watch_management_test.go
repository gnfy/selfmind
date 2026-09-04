package control

import (
	"context"
	"testing"
	"time"
)

func TestExternalWatchManagementIsPersonScoped(t *testing.T) {
	ctx := context.Background()
	store, owner, task, run := newRecoveryFixture(t)
	other, err := store.ResolveOrCreateAccount(ctx, owner.TenantID, "cli", "other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	otherTask, err := store.CreateTask(ctx, TaskCreate{
		TenantID: other.TenantID, PersonID: other.PersonID, Title: "other work", Channel: "cli-other",
	})
	if err != nil {
		t.Fatal(err)
	}
	otherRun, err := store.StartRun(ctx, otherTask, "cli-other", "other work")
	if err != nil {
		t.Fatal(err)
	}

	ownerWatch := createManagedWatch(t, store, owner, task, run, "owner watch")
	otherWatch := createManagedWatch(t, store, other, otherTask, otherRun, "other watch")

	listed, err := store.ListExternalWatchesForPerson(ctx, owner.TenantID, owner.PersonID, ExternalWatchListAll, 10, 0)
	if err != nil || len(listed) != 1 || listed[0].ID != ownerWatch.ID {
		t.Fatalf("owner list = %+v err=%v", listed, err)
	}
	if found, ambiguous, err := store.ResolveExternalWatchForPerson(ctx, owner.TenantID, owner.PersonID, otherWatch.ID); err != nil || ambiguous || found != nil {
		t.Fatalf("other person's watch resolved: found=%+v ambiguous=%v err=%v", found, ambiguous, err)
	}
	if cancelled, err := store.CancelExternalWatchForPerson(ctx, owner.TenantID, owner.PersonID, otherWatch.ID); err != nil || cancelled {
		t.Fatalf("other person's watch cancelled: cancelled=%v err=%v", cancelled, err)
	}
	if stored, err := store.GetExternalWatch(ctx, owner.TenantID, otherWatch.ID); err != nil || stored == nil || stored.Status != ExternalWatchPending {
		t.Fatalf("other watch changed: %+v err=%v", stored, err)
	}
}

func TestCancelExternalWatchParksOnlyAfterLastActiveWatch(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_external"); err != nil {
		t.Fatal(err)
	}
	first := createManagedWatch(t, store, identity, task, run, "first")
	second := createManagedWatch(t, store, identity, task, run, "second")
	timeline := NewWorkTimeline(store)

	if cancelled, err := store.CancelExternalWatchForPerson(ctx, identity.TenantID, identity.PersonID, first.ID); err != nil || !cancelled {
		t.Fatalf("first cancel = %v err=%v", cancelled, err)
	}
	attention, err := timeline.Attention(ctx, identity.TenantID, identity.PersonID, 10)
	if err != nil || len(attention) != 1 || attention[0].Activity != ThreadActivityMonitoring {
		t.Fatalf("attention after first cancel = %+v err=%v", attention, err)
	}
	if cancelled, err := store.CancelExternalWatchForPerson(ctx, identity.TenantID, identity.PersonID, second.ID); err != nil || !cancelled {
		t.Fatalf("second cancel = %v err=%v", cancelled, err)
	}
	attention, err = timeline.Attention(ctx, identity.TenantID, identity.PersonID, 10)
	if err != nil || len(attention) != 1 || attention[0].Activity != ThreadActivityResumable || attention[0].RunID != run.ID {
		t.Fatalf("cancelled watchers must leave the exact run resumable: %+v err=%v", attention, err)
	}
	stored, err := store.GetExternalWatch(ctx, identity.TenantID, second.ID)
	if err != nil || stored == nil || stored.Status != ExternalWatchCancelled || !stored.Finalized || !stored.Notified {
		t.Fatalf("cancelled watch = %+v err=%v", stored, err)
	}
	if cancelled, err := store.CancelExternalWatchForPerson(ctx, identity.TenantID, identity.PersonID, second.ID); err != nil || cancelled {
		t.Fatalf("repeat cancel = %v err=%v", cancelled, err)
	}
}

func TestResolveExternalWatchPrefixRejectsAmbiguity(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	first := createManagedWatch(t, store, identity, task, run, "first")
	second := createManagedWatch(t, store, identity, task, run, "second")
	// UUIDs differ too early to naturally collide in a deterministic test, so
	// give both rows a controlled display prefix while preserving valid ids.
	firstID := "watch_deadbeef-1111-4111-8111-111111111111"
	secondID := "watch_deadbeef-2222-4222-8222-222222222222"
	if _, err := store.db.ExecContext(ctx, "UPDATE external_watches SET id = ? WHERE id = ?", firstID, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE external_watches SET id = ? WHERE id = ?", secondID, second.ID); err != nil {
		t.Fatal(err)
	}
	if found, ambiguous, err := store.ResolveExternalWatchForPerson(ctx, identity.TenantID, identity.PersonID, "watch_deadbeef"); err != nil || !ambiguous || found != nil {
		t.Fatalf("ambiguous prefix = found %+v ambiguous=%v err=%v", found, ambiguous, err)
	}
	if found, ambiguous, err := store.ResolveExternalWatchForPerson(ctx, identity.TenantID, identity.PersonID, firstID); err != nil || ambiguous || found == nil || found.ID != firstID {
		t.Fatalf("full id = found %+v ambiguous=%v err=%v", found, ambiguous, err)
	}
}

func createManagedWatch(t *testing.T, store *Store, identity *IdentityContext, task *Task, run *Run, description string) *ExternalWatch {
	t.Helper()
	watch, err := store.CreateExternalWatch(context.Background(), ExternalWatch{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		TaskID: task.ID, RunID: run.ID, Channel: "cli", Description: description,
		CWD: t.TempDir(), Command: "check", SuccessPattern: "SUCCEEDED",
		TimeoutAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	return watch
}
