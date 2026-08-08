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

func TestExternalWatchV2PersistsAndBacksOffUnchangedOutput(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	watch, err := store.CreateExternalWatch(ctx, ExternalWatch{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		TaskID: task.ID, RunID: run.ID, Channel: "cli",
		CWD: t.TempDir(), Command: "check", SpecVersion: 2,
		TargetPattern: "PENDING_APPROVAL", TerminalSuccessPattern: "SUCCEEDED",
		TerminalFailurePattern: "FAILED", IntervalSeconds: 5,
		TimeoutAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := func(output string) *ExternalWatch {
		if _, err := store.db.ExecContext(ctx, `UPDATE external_watches SET next_check_at = 0 WHERE id = ?`, watch.ID); err != nil {
			t.Fatal(err)
		}
		current, err := store.GetExternalWatch(ctx, watch.TenantID, watch.ID)
		if err != nil {
			t.Fatal(err)
		}
		if claimed, err := store.ClaimExternalWatch(ctx, *current); err != nil || !claimed {
			t.Fatalf("claim=%v err=%v", claimed, err)
		}
		if err := store.RecordExternalWatchCheck(ctx, watch.TenantID, watch.ID, output, ""); err != nil {
			t.Fatal(err)
		}
		current, err = store.GetExternalWatch(ctx, watch.TenantID, watch.ID)
		if err != nil {
			t.Fatal(err)
		}
		return current
	}
	if got := checkpoint("QUEUED"); got.SpecVersion != 2 || got.CurrentIntervalSeconds != 5 {
		t.Fatalf("first checkpoint=%+v", got)
	}
	if got := checkpoint("QUEUED"); got.CurrentIntervalSeconds != 10 {
		t.Fatalf("second interval=%d want 10", got.CurrentIntervalSeconds)
	}
	if got := checkpoint("RUNNING"); got.CurrentIntervalSeconds != 5 {
		t.Fatalf("changed output interval=%d want reset to 5", got.CurrentIntervalSeconds)
	}
}

func TestExternalWatchV2TargetRequiresBothTerminalOutcomes(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	base := ExternalWatch{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		TaskID: task.ID, RunID: run.ID, Channel: "cli",
		CWD: t.TempDir(), Command: "check", SpecVersion: 2,
		TargetPattern: "PENDING_APPROVAL", TimeoutAt: time.Now().Add(time.Hour),
	}
	for name, mutate := range map[string]func(*ExternalWatch){
		"neither terminal": func(*ExternalWatch) {},
		"success only":     func(w *ExternalWatch) { w.TerminalSuccessPattern = "SUCCEEDED" },
		"failure only":     func(w *ExternalWatch) { w.TerminalFailurePattern = "FAILED" },
	} {
		t.Run(name, func(t *testing.T) {
			watch := base
			mutate(&watch)
			if _, err := store.CreateExternalWatch(ctx, watch); err == nil {
				t.Fatal("target-based V2 watch without both terminal outcomes must be rejected")
			}
		})
	}
	complete := base
	complete.TerminalSuccessPattern = "SUCCEEDED"
	complete.TerminalFailurePattern = "FAILED"
	if _, err := store.CreateExternalWatch(ctx, complete); err != nil {
		t.Fatalf("complete V2 verdict contract rejected: %v", err)
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

// The circuit breaker's input: an identical failure must accumulate, a different
// one must reset, and a recovered check must clear the streak. Without this a
// watch that can never observe anything keeps polling until its deadline.
func TestExternalWatchFailureStreak(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	watch, err := store.CreateExternalWatch(ctx, ExternalWatch{
		TenantID:              identity.TenantID,
		PersonID:              identity.PersonID,
		TaskID:                task.ID,
		RunID:                 run.ID,
		CWD:                   t.TempDir(),
		Command:               "check-build",
		SuccessPattern:        "SUCCEEDED",
		IntervalSeconds:       5,
		CommandTimeoutSeconds: 10,
		TimeoutAt:             time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	due, err := store.ListDueExternalWatches(ctx, 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("due = %+v err=%v", due, err)
	}
	if claimed, err := store.ClaimExternalWatch(ctx, due[0]); err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}

	for want := 1; want <= 3; want++ {
		streak, err := store.RecordExternalWatchFailure(ctx, watch.TenantID, watch.ID,
			"bwrap: Can't mkdir parents", "exit status 1", "credential_state_readonly", "sig-a")
		if err != nil {
			t.Fatal(err)
		}
		if streak != want {
			t.Fatalf("streak = %d, want %d", streak, want)
		}
	}

	streak, err := store.RecordExternalWatchFailure(ctx, watch.TenantID, watch.ID,
		"connection reset", "exit status 1", "network", "sig-b")
	if err != nil {
		t.Fatal(err)
	}
	if streak != 1 {
		t.Fatalf("a different failure must restart the streak, got %d", streak)
	}

	stored, err := store.GetExternalWatch(ctx, watch.TenantID, watch.ID)
	if err != nil || stored == nil {
		t.Fatalf("get watch: %+v err=%v", stored, err)
	}
	if stored.FailureClass != "network" || stored.ConsecutiveFailures != 1 {
		t.Fatalf("stored failure state = %+v", stored)
	}

	if err := store.RecordExternalWatchCheck(ctx, watch.TenantID, watch.ID, "WORKING", ""); err != nil {
		t.Fatal(err)
	}
	stored, err = store.GetExternalWatch(ctx, watch.TenantID, watch.ID)
	if err != nil || stored == nil {
		t.Fatalf("get watch: %+v err=%v", stored, err)
	}
	if stored.FailureClass != "" || stored.ConsecutiveFailures != 0 {
		t.Fatalf("a healthy check must clear the streak: %+v", stored)
	}
}
