package control

import (
	"context"
	"testing"
	"time"
)

func TestExternalWatchLifecycle(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	watch, err := store.CreateExternalWatch(ctx, ExternalWatch{
		TenantID:              identity.TenantID,
		PersonID:              identity.PersonID,
		TaskID:                task.ID,
		RunID:                 run.ID,
		Channel:               "cli",
		Description:           "CI build",
		CWD:                   t.TempDir(),
		Command:               "check-build",
		SuccessPattern:        "SUCCEEDED",
		FailurePattern:        "FAILED",
		IntervalSeconds:       5,
		CommandTimeoutSeconds: 10,
		TimeoutAt:             time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	due, err := store.ListDueExternalWatches(ctx, 10)
	if err != nil || len(due) != 1 || due[0].ID != watch.ID {
		t.Fatalf("due = %+v err=%v", due, err)
	}
	claimed, err := store.ClaimExternalWatch(ctx, due[0])
	if err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	if claimed, err := store.ClaimExternalWatch(ctx, due[0]); err != nil || claimed {
		t.Fatalf("duplicate claim: claimed=%v err=%v", claimed, err)
	}
	if err := store.RecordExternalWatchCheck(ctx, watch.TenantID, watch.ID, "still running", ""); err != nil {
		t.Fatal(err)
	}
	finished, err := store.FinishExternalWatch(ctx, watch.TenantID, watch.ID, ExternalWatchSucceeded, "SUCCEEDED", "")
	if err != nil || !finished {
		t.Fatalf("finish: finished=%v err=%v", finished, err)
	}
	if finished, err := store.FinishExternalWatch(ctx, watch.TenantID, watch.ID, ExternalWatchFailed, "", "late failure"); err != nil || finished {
		t.Fatalf("terminal watch changed twice: finished=%v err=%v", finished, err)
	}
	counts, err := store.CountExternalWatchesByStatus(ctx, identity.TenantID, identity.PersonID)
	if err != nil || counts[ExternalWatchSucceeded] != 1 {
		t.Fatalf("counts = %+v err=%v", counts, err)
	}
}

func TestExternalWatchVerdictRevision(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	watch, err := store.CreateExternalWatch(ctx, ExternalWatch{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		TaskID: task.ID, RunID: run.ID, Channel: "cli",
		CWD: t.TempDir(), Command: "check", SuccessPattern: "^SUCCESS$",
		TimeoutAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the historical misjudgment: watch finished timed_out while its
	// recorded output already showed SUCCESS.
	if finished, err := store.FinishExternalWatch(ctx, watch.TenantID, watch.ID, ExternalWatchTimedOut, "SUCCESS\n", "watch deadline reached"); err != nil || !finished {
		t.Fatalf("finish timed_out: %v %v", finished, err)
	}
	listed, err := store.ListExternalWatchesFinishedSince(ctx, ExternalWatchTimedOut, time.Now().Add(-time.Hour), 10)
	if err != nil || len(listed) != 1 || listed[0].ID != watch.ID {
		t.Fatalf("list finished = %+v err=%v", listed, err)
	}
	revised, err := store.ReviseExternalWatchVerdict(ctx, watch.TenantID, watch.ID, ExternalWatchTimedOut, ExternalWatchSucceeded)
	if err != nil || !revised {
		t.Fatalf("revise: %v %v", revised, err)
	}
	// The CAS must not revise twice.
	if revised, err := store.ReviseExternalWatchVerdict(ctx, watch.TenantID, watch.ID, ExternalWatchTimedOut, ExternalWatchSucceeded); err != nil || revised {
		t.Fatalf("double revise: %v %v", revised, err)
	}
	counts, err := store.CountExternalWatchesByStatus(ctx, identity.TenantID, identity.PersonID)
	if err != nil || counts[ExternalWatchSucceeded] != 1 || counts[ExternalWatchTimedOut] != 0 {
		t.Fatalf("counts = %+v err=%v", counts, err)
	}
	if _, err := store.ReviseExternalWatchVerdict(ctx, watch.TenantID, watch.ID, ExternalWatchTimedOut, ExternalWatchRunning); err == nil {
		t.Fatal("revision to a non-terminal status must be rejected")
	}
}

func TestExternalWatchSingleDeadlineExtension(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	watch, err := store.CreateExternalWatch(ctx, ExternalWatch{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		TaskID: task.ID, RunID: run.ID, Channel: "cli",
		CWD: t.TempDir(), Command: "check", SuccessPattern: "^SUCCESS$",
		TimeoutAt: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	due, err := store.ListDueExternalWatches(ctx, 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("due = %+v err=%v", due, err)
	}
	if claimed, err := store.ClaimExternalWatch(ctx, due[0]); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	until := time.Now().Add(30 * time.Minute)
	extended, err := store.ExtendExternalWatchDeadline(ctx, watch.TenantID, watch.ID, until, "WORKING\n")
	if err != nil || !extended {
		t.Fatalf("extend: %v %v", extended, err)
	}
	// Exactly one grant: the second extension is refused.
	if extended, err := store.ExtendExternalWatchDeadline(ctx, watch.TenantID, watch.ID, until.Add(time.Hour), "WORKING\n"); err != nil || extended {
		t.Fatalf("second extend must be refused: %v %v", extended, err)
	}
	var extensions int
	var timeoutAt int64
	if err := store.db.QueryRowContext(ctx, `SELECT extensions, timeout_at FROM external_watches WHERE id = ?`, watch.ID).Scan(&extensions, &timeoutAt); err != nil {
		t.Fatal(err)
	}
	if extensions != 1 || timeoutAt != until.Unix() {
		t.Fatalf("extensions=%d timeout_at=%d want 1/%d", extensions, timeoutAt, until.Unix())
	}
}

func TestQueuedTaskCarriesTaskID(t *testing.T) {
	ctx := context.Background()
	store, identity, task, _ := newRecoveryFixture(t)
	queued, err := store.EnqueueQueued(ctx, QueuedTask{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Channel:  "cli",
		Platform: "cli",
		Content:  "finalize the external operation",
		TaskID:   task.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	next, err := store.NextQueued(ctx, identity.TenantID, identity.PersonID)
	if err != nil || next == nil {
		t.Fatalf("next: %+v err=%v", next, err)
	}
	if next.ID != queued.ID || next.TaskID != task.ID {
		t.Fatalf("queued row lost task pin: %+v", next)
	}
}

// TestExternalWatchFinalizationCompensation pins the at-least-once contract:
// the terminal-status CAS and the completion side effects are separate steps,
// so a terminal watch stays visible to the boot compensation scan until it is
// explicitly marked finalized, and the mark itself is once-only.
func TestExternalWatchFinalizationCompensation(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	watch, err := store.CreateExternalWatch(ctx, ExternalWatch{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		TaskID: task.ID, RunID: run.ID, Channel: "cli",
		CWD: t.TempDir(), Command: "check", SuccessPattern: "^SUCCESS$",
		TimeoutAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if finished, err := store.FinishExternalWatch(ctx, watch.TenantID, watch.ID, ExternalWatchSucceeded, "SUCCESS\n", ""); err != nil || !finished {
		t.Fatalf("finish: %v %v", finished, err)
	}
	pending, err := store.ListUnfinalizedExternalWatches(ctx, time.Now().Add(-time.Hour), 10)
	if err != nil || len(pending) != 1 || pending[0].ID != watch.ID {
		t.Fatalf("unfinalized = %+v err=%v", pending, err)
	}
	if marked, err := store.MarkExternalWatchFinalized(ctx, watch.TenantID, watch.ID); err != nil || !marked {
		t.Fatalf("mark: %v %v", marked, err)
	}
	if marked, err := store.MarkExternalWatchFinalized(ctx, watch.TenantID, watch.ID); err != nil || marked {
		t.Fatalf("double mark must lose: %v %v", marked, err)
	}
	if pending, err := store.ListUnfinalizedExternalWatches(ctx, time.Now().Add(-time.Hour), 10); err != nil || len(pending) != 0 {
		t.Fatalf("finalized watch resurfaced: %+v err=%v", pending, err)
	}
	unnotified, err := store.ListUnnotifiedExternalWatches(ctx, time.Now().Add(-time.Hour), 10)
	if err != nil || len(unnotified) != 1 || unnotified[0].ID != watch.ID {
		t.Fatalf("unnotified = %+v err=%v", unnotified, err)
	}
	if marked, err := store.MarkExternalWatchNotified(ctx, watch.TenantID, watch.ID); err != nil || !marked {
		t.Fatalf("mark notified: %v %v", marked, err)
	}
	if marked, err := store.MarkExternalWatchNotified(ctx, watch.TenantID, watch.ID); err != nil || marked {
		t.Fatalf("double notification mark must lose: %v %v", marked, err)
	}
	if unnotified, err := store.ListUnnotifiedExternalWatches(ctx, time.Now().Add(-time.Hour), 10); err != nil || len(unnotified) != 0 {
		t.Fatalf("notified watch resurfaced: %+v err=%v", unnotified, err)
	}
}

// A revised verdict is a new outcome: revision must re-open finalization so
// the corrected side effects run via the compensation path.
func TestExternalWatchRevisionReopensFinalization(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	watch, err := store.CreateExternalWatch(ctx, ExternalWatch{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		TaskID: task.ID, RunID: run.ID, Channel: "cli",
		CWD: t.TempDir(), Command: "check", SuccessPattern: "^SUCCESS$",
		TimeoutAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if finished, err := store.FinishExternalWatch(ctx, watch.TenantID, watch.ID, ExternalWatchTimedOut, "SUCCESS\n", "watch deadline reached"); err != nil || !finished {
		t.Fatalf("finish: %v %v", finished, err)
	}
	if marked, err := store.MarkExternalWatchFinalized(ctx, watch.TenantID, watch.ID); err != nil || !marked {
		t.Fatalf("mark timeout finalize: %v %v", marked, err)
	}
	if marked, err := store.MarkExternalWatchNotified(ctx, watch.TenantID, watch.ID); err != nil || !marked {
		t.Fatalf("mark timeout notified: %v %v", marked, err)
	}
	if revised, err := store.ReviseExternalWatchVerdict(ctx, watch.TenantID, watch.ID, ExternalWatchTimedOut, ExternalWatchSucceeded); err != nil || !revised {
		t.Fatalf("revise: %v %v", revised, err)
	}
	pending, err := store.ListUnfinalizedExternalWatches(ctx, time.Now().Add(-time.Hour), 10)
	if err != nil || len(pending) != 1 || pending[0].Status != ExternalWatchSucceeded {
		t.Fatalf("revised watch must need finalization: %+v err=%v", pending, err)
	}
	if pending[0].Notified {
		t.Fatal("revised verdict must reopen notification")
	}
}

// TestExternalWatchFinalizationIdempotencyKeys pins the replay-safety
// contract by ROW COUNTS, not just flags: the same verdict replayed any
// number of times materializes one queue row and one completed event, while a
// verdict revision earns exactly one fresh set under new keys.
func TestExternalWatchFinalizationIdempotencyKeys(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	watch, err := store.CreateExternalWatch(ctx, ExternalWatch{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		TaskID: task.ID, RunID: run.ID, Channel: "cli",
		CWD: t.TempDir(), Command: "check", SuccessPattern: "^SUCCESS$",
		TimeoutAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Queue product: same stable key three times -> one row, all calls succeed.
	key := "external-watch:" + watch.ID + ":r1:finalization"
	for i := 0; i < 3; i++ {
		if _, err := store.EnqueueQueued(ctx, QueuedTask{
			TenantID: identity.TenantID, PersonID: identity.PersonID,
			Channel: "cli", Platform: "cli", Content: "finalize",
			TaskID: task.ID, IdempotencyKey: key,
		}); err != nil {
			t.Fatalf("replayed enqueue %d must succeed: %v", i, err)
		}
	}
	var queueRows int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM task_queue WHERE idempotency_key = ?`, key).Scan(&queueRows); err != nil {
		t.Fatal(err)
	}
	if queueRows != 1 {
		t.Fatalf("queue rows = %d, want 1", queueRows)
	}
	// A revised verdict's key is new: one more row is allowed.
	if _, err := store.EnqueueQueued(ctx, QueuedTask{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		Channel: "cli", Platform: "cli", Content: "finalize revision",
		TaskID: task.ID, IdempotencyKey: "external-watch:" + watch.ID + ":r2:finalization",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM task_queue WHERE idempotency_key LIKE ?`, "external-watch:"+watch.ID+"%").Scan(&queueRows); err != nil {
		t.Fatal(err)
	}
	if queueRows != 2 {
		t.Fatalf("queue rows after revision = %d, want 2", queueRows)
	}

	// Event product: the database invariant, not a check-then-insert caller
	// convention, gates replays per revision.
	payload := []byte(`{"watch_id":"` + watch.ID + `","revision":1,"status":"succeeded"}`)
	eventKey := "external-watch:" + watch.ID + ":r1:completed"
	firstEvent, err := store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: run.ID, Type: "external_watch.completed", Visibility: "task", Channel: "cli", Payload: payload, IdempotencyKey: eventKey})
	if err != nil {
		t.Fatal(err)
	}
	secondEvent, err := store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: run.ID, Type: "external_watch.completed", Visibility: "task", Channel: "cli", Payload: payload, IdempotencyKey: eventKey})
	if err != nil {
		t.Fatal(err)
	}
	if firstEvent.ID != secondEvent.ID || firstEvent.Cursor != secondEvent.Cursor {
		t.Fatalf("duplicate event = (%s,%d), want (%s,%d)", secondEvent.ID, secondEvent.Cursor, firstEvent.ID, firstEvent.Cursor)
	}
	var eventRows int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM task_events WHERE idempotency_key = ?`, eventKey).Scan(&eventRows); err != nil {
		t.Fatal(err)
	}
	if eventRows != 1 {
		t.Fatalf("event rows = %d, want 1", eventRows)
	}
	if _, err := store.AppendEvent(ctx, Event{
		TaskID: task.ID, RunID: run.ID, Type: "external_watch.completed", Visibility: "task", Channel: "cli",
		Payload:        []byte(`{"watch_id":"` + watch.ID + `","revision":2,"status":"succeeded"}`),
		IdempotencyKey: "external-watch:" + watch.ID + ":r2:completed",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM task_events WHERE idempotency_key LIKE ?`, "external-watch:"+watch.ID+"%:completed").Scan(&eventRows); err != nil {
		t.Fatal(err)
	}
	if eventRows != 2 {
		t.Fatalf("event rows after revision = %d, want 2", eventRows)
	}

	// Revision bookkeeping: revise bumps verdict_revision and re-opens finalization.
	if finished, err := store.FinishExternalWatch(ctx, watch.TenantID, watch.ID, ExternalWatchTimedOut, "SUCCESS\n", "watch deadline reached"); err != nil || !finished {
		t.Fatalf("finish: %v %v", finished, err)
	}
	if marked, err := store.MarkExternalWatchFinalized(ctx, watch.TenantID, watch.ID); err != nil || !marked {
		t.Fatalf("mark: %v %v", marked, err)
	}
	if revised, err := store.ReviseExternalWatchVerdict(ctx, watch.TenantID, watch.ID, ExternalWatchTimedOut, ExternalWatchSucceeded); err != nil || !revised {
		t.Fatalf("revise: %v %v", revised, err)
	}
	pending, err := store.ListUnfinalizedExternalWatches(ctx, time.Now().Add(-time.Hour), 10)
	if err != nil || len(pending) != 1 || pending[0].VerdictRevision != 2 {
		t.Fatalf("revised watch revision = %+v err=%v", pending, err)
	}

	// Cancelled watches owe no side effects: born finalized.
	cancelled, err := store.CreateExternalWatch(ctx, ExternalWatch{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		TaskID: task.ID, RunID: run.ID, Channel: "cli",
		CWD: t.TempDir(), Command: "check", SuccessPattern: "^SUCCESS$",
		TimeoutAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if finished, err := store.FinishExternalWatch(ctx, cancelled.TenantID, cancelled.ID, ExternalWatchCancelled, "", "user cancelled"); err != nil || !finished {
		t.Fatalf("cancel: %v %v", finished, err)
	}
	if pending, err := store.ListUnfinalizedExternalWatches(ctx, time.Now().Add(-time.Hour), 10); err != nil || len(pending) != 1 {
		t.Fatalf("cancelled watch must not need compensation: %+v err=%v", pending, err)
	}
}
