package control

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestClaimedQueueRunCreationBindsAtomically(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "queued", Channel: "session-a"})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := store.EnqueueQueued(ctx, QueuedTask{TenantID: identity.TenantID, PersonID: identity.PersonID, Platform: "cli", PlatformUserID: "local", Channel: "session-a", Content: "work"})
	if err != nil {
		t.Fatal(err)
	}
	token, claimed, err := store.ClaimQueued(ctx, identity.TenantID, queued.ID, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claim: token=%q claimed=%v err=%v", token, claimed, err)
	}
	opts := StartRunOptions{QueueID: queued.ID, QueueClaimToken: token}
	// Fail after the queue row has been bound, while inserting the Run's first
	// work unit. The transaction must leave neither the Run nor its binding.
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_work_unit BEFORE INSERT ON run_work_units BEGIN SELECT RAISE(ABORT, 'injected work-unit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartRunWithOptions(ctx, task, "session-a", "work", opts); err == nil {
		t.Fatal("injected post-bind failure was ignored")
	}
	row, err := store.GetQueued(ctx, identity.TenantID, queued.ID)
	if err != nil || row == nil || row.RunID != "" || row.Status != QueueStatusStarted {
		t.Fatalf("post-bind rollback left queue changed: row=%+v err=%v", row, err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER fail_work_unit`); err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRunWithOptions(ctx, task, "session-a", "work", opts)
	if err != nil {
		t.Fatal(err)
	}
	row, err = store.GetQueued(ctx, identity.TenantID, queued.ID)
	if err != nil || row == nil || row.RunID != run.ID {
		t.Fatalf("committed Run has no exact queue binding: row=%+v run=%+v err=%v", row, run, err)
	}
	if changed, err := store.RequeueUnboundClaim(ctx, identity.TenantID, queued.ID, token); err != nil || changed {
		t.Fatalf("bound Run was requeued: changed=%v err=%v", changed, err)
	}
	if changed, err := store.FinishQueuedClaim(ctx, identity.TenantID, queued.ID, token, QueueStatusDone); err != nil || !changed {
		t.Fatalf("bound claimant could not finish row: changed=%v err=%v", changed, err)
	}
}

func TestStaleQueueClaimCreatesNoRunAndCannotSettleNewOwner(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "queued", Channel: "session-a"})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := store.EnqueueQueued(ctx, QueuedTask{TenantID: identity.TenantID, PersonID: identity.PersonID, Platform: "cli", PlatformUserID: "local", Channel: "session-a", Content: "work"})
	if err != nil {
		t.Fatal(err)
	}
	oldToken, claimed, err := store.ClaimQueued(ctx, identity.TenantID, queued.ID, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("first claim: claimed=%v err=%v", claimed, err)
	}
	if changed, err := store.RequeueUnboundClaim(ctx, identity.TenantID, queued.ID, oldToken); err != nil || !changed {
		t.Fatalf("release first claim: changed=%v err=%v", changed, err)
	}
	newToken, claimed, err := store.ClaimQueued(ctx, identity.TenantID, queued.ID, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("second claim: claimed=%v err=%v", claimed, err)
	}
	if _, err := store.StartRunWithOptions(ctx, task, "session-a", "old attempt", StartRunOptions{QueueID: queued.ID, QueueClaimToken: oldToken}); !errors.Is(err, ErrQueueClaimLost) {
		t.Fatalf("stale claim error=%v, want ErrQueueClaimLost", err)
	}
	if changed, err := store.RequeueUnboundClaim(ctx, identity.TenantID, queued.ID, oldToken); err != nil || changed {
		t.Fatalf("old claimant requeued new owner: changed=%v err=%v", changed, err)
	}
	run, err := store.StartRunWithOptions(ctx, task, "session-a", "new attempt", StartRunOptions{QueueID: queued.ID, QueueClaimToken: newToken})
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := store.FinishQueuedClaim(ctx, identity.TenantID, queued.ID, oldToken, QueueStatusDone); err != nil || changed {
		t.Fatalf("old claimant settled new owner: changed=%v err=%v", changed, err)
	}
	row, err := store.GetQueued(ctx, identity.TenantID, queued.ID)
	if err != nil || row == nil || row.Status != QueueStatusStarted || row.RunID != run.ID {
		t.Fatalf("new claim was overwritten: row=%+v err=%v", row, err)
	}
}

func TestBootDoesNotBlindlyReplayBoundRunWithUnknownEffect(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "release", Channel: "session-a"})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := store.EnqueueQueued(ctx, QueuedTask{TenantID: identity.TenantID, PersonID: identity.PersonID, Platform: "cli", Channel: "session-a", Content: "dispatch release"})
	if err != nil {
		t.Fatal(err)
	}
	token, claimed, err := store.ClaimQueued(ctx, identity.TenantID, queued.ID, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	run, err := store.StartRunWithOptions(ctx, task, "session-a", "dispatch release", StartRunOptions{QueueID: queued.ID, QueueClaimToken: token})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: run.ID, Type: "tool.completed", Channel: "session-a", Payload: []byte(`{"effect_certainty":"unknown"}`)}); err != nil {
		t.Fatal(err)
	}
	if recovered, err := store.MarkInterruptedRuns(ctx, 0); err != nil || recovered != 1 {
		t.Fatalf("interrupt crashed Run: recovered=%d err=%v", recovered, err)
	}
	if requeued, dropped, err := store.RequeueStartedQueued(ctx); err != nil || requeued != 0 || dropped != 1 {
		t.Fatalf("boot replayed uncertain effect: requeued=%d dropped=%d err=%v", requeued, dropped, err)
	}
	row, err := store.GetQueued(ctx, identity.TenantID, queued.ID)
	if err != nil || row == nil || row.Status != QueueStatusFailed || row.RunID != run.ID {
		t.Fatalf("boot lost uncertain Run binding: row=%+v err=%v", row, err)
	}
	stored, err := store.GetRun(ctx, identity.TenantID, run.ID)
	if err != nil || stored == nil || stored.Status != "interrupted" {
		t.Fatalf("Run was not left for exact recovery: run=%+v err=%v", stored, err)
	}
}

func TestLegacyQueuedBoundRunCannotBeClaimedOrReplayed(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "alice", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "legacy interrupted work"})
	if err != nil {
		t.Fatal(err)
	}
	row, err := store.EnqueueQueued(ctx, QueuedTask{TenantID: identity.TenantID, PersonID: identity.PersonID, Content: "dispatch once"})
	if err != nil {
		t.Fatal(err)
	}
	token, claimed, err := store.ClaimQueued(ctx, identity.TenantID, row.ID, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("initial claim = %q/%v, %v", token, claimed, err)
	}
	run, err := store.StartRunWithOptions(ctx, task, "cli", "dispatch once", StartRunOptions{QueueID: row.ID, QueueClaimToken: token})
	if err != nil {
		t.Fatal(err)
	}
	// An older daemon reopened the queue row during shutdown after the Run
	// started. The new daemon must preserve the exact Run and its effects.
	if err := store.MarkQueued(ctx, identity.TenantID, row.ID, QueueStatusQueued); err != nil {
		t.Fatal(err)
	}
	if next, err := store.NextQueued(ctx, identity.TenantID, identity.PersonID); err != nil || next != nil {
		t.Fatalf("bound row offered for drain = %+v, %v", next, err)
	}
	if _, claimed, err := store.ClaimQueued(ctx, identity.TenantID, row.ID, time.Minute); err != nil || claimed {
		t.Fatalf("bound row claimed again = %v, %v", claimed, err)
	}
	if requeued, dropped, err := store.RequeueStartedQueued(ctx); err != nil || requeued != 0 || dropped != 1 {
		t.Fatalf("boot recovery = %d/%d, %v; want quarantine", requeued, dropped, err)
	}
	stored, err := store.GetQueued(ctx, identity.TenantID, row.ID)
	if err != nil || stored == nil || stored.Status != QueueStatusFailed || stored.RunID != run.ID {
		t.Fatalf("legacy row lost exact Run: %+v, %v", stored, err)
	}
}
