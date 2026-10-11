package control

import (
	"context"
	"strings"
	"testing"
	"time"
)

func parkedProviderFixture(t *testing.T) (*Store, *IdentityContext, *Task, *Run, *QueuedTask) {
	t.Helper()
	store, owner, task, run := newRecoveryFixture(t)
	q := parkProviderFixtureRun(t, store, owner, task, run)
	return store, owner, task, run, q
}

func parkProviderFixtureRun(t *testing.T, store *Store, owner *IdentityContext, task *Task, run *Run) *QueuedTask {
	t.Helper()
	ctx := context.Background()
	if err := store.SaveLoopCheckpoint(ctx, LoopCheckpointRecord{TenantID: owner.TenantID, PersonID: owner.PersonID, TaskID: task.ID, RunID: run.ID,
		Outcome: "continue_model", Detail: "provider_request", Snapshot: []byte(`[{"role":"user","content":"keep the original work"}]`)}); err != nil {
		t.Fatal(err)
	}
	q := &QueuedTask{TenantID: owner.TenantID, PersonID: owner.PersonID, TaskID: task.ID, ReplyToRunID: run.ID,
		Content: "continue the exact checkpoint", Channel: run.Channel, Platform: "cli", PlatformUserID: "local", IdempotencyKey: "provider-wait:" + run.ID, NotBefore: time.Now().Add(-time.Second)}
	if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{Identity: *owner, RunID: run.ID, TaskID: task.ID, RunStatus: "waiting_external",
		ExpectedRunStatus: "running", RequireCheckpoint: true, Continuation: q, Event: Event{Payload: []byte(`{"outcome":{"status":"waiting_external","completion_reason":"provider_wait"},"provider_wait":{"reason":"rate_limit"}}`)}}); err != nil {
		t.Fatal(err)
	}
	return q
}

func TestProviderCancellationDoesNotCompletePendingPlanOrUncertainEffect(t *testing.T) {
	store, owner, task, run := newRecoveryFixture(t)
	ctx := context.Background()
	plan, err := store.SyncRunPlan(ctx, owner.TenantID, run.ID, "unfinished deployment", []RunPlanStepInput{{Step: "Observe the deployed state", Status: "in_progress"}})
	if err != nil {
		t.Fatal(err)
	}
	dispatchedEffect(t, store, owner, run, "deploy", false, "service:alpha")
	returnedEffect(t, store, owner, run, "deploy", 1)
	parkProviderFixtureRun(t, store, owner, task, run)
	if cancelled, err := store.CancelParkedProviderRun(ctx, owner.TenantID, owner.PersonID, run.ID); err != nil || !cancelled {
		t.Fatalf("cancel=%v %v", cancelled, err)
	}
	stored, err := store.LatestRunPlan(ctx, owner.TenantID, run.ID)
	if err != nil || stored == nil || stored.Version != plan.Plan.Version || stored.Steps[0].Status != "in_progress" {
		t.Fatalf("cancellation rewrote plan obligations: %+v %v", stored, err)
	}
	if got := effectDisposition(t, store, owner, run, "deploy"); got != ExternalClaimLeftUncertain {
		t.Fatalf("cancellation fabricated observation of the effect: %s", got)
	}
	if err := store.ValidateRunCompletion(ctx, owner.TenantID, run.ID); err == nil {
		t.Fatal("cancelled execution was accepted as completed business work")
	}
}

func TestProviderCancellationRollsBackWhenCancellationCannotBeRecorded(t *testing.T) {
	for _, action := range []string{"stop", "clear", "drop"} {
		t.Run(action, func(t *testing.T) {
			store, owner, _, run, q := parkedProviderFixture(t)
			ctx := context.Background()
			if _, err := store.db.Exec(`CREATE TRIGGER reject_cancel_event BEFORE INSERT ON task_events WHEN NEW.type='run.cancelled'
		BEGIN SELECT RAISE(ABORT,'cannot record cancellation'); END`); err != nil {
				t.Fatal(err)
			}
			var err error
			switch action {
			case "stop":
				var cancelled bool
				cancelled, err = store.CancelParkedProviderRun(ctx, owner.TenantID, owner.PersonID, run.ID)
				if cancelled {
					t.Fatal("failed stop reported cancellation")
				}
			case "clear":
				var n int
				n, err = store.ClearQueued(ctx, owner.TenantID, owner.PersonID)
				if n != 0 {
					t.Fatalf("failed clear reported %d cancellations", n)
				}
			case "drop":
				var cancelled bool
				cancelled, err = store.CancelQueued(ctx, owner.TenantID, owner.PersonID, q.ID)
				if cancelled {
					t.Fatal("failed drop reported cancellation")
				}
			}
			if err == nil || !strings.Contains(err.Error(), "cannot record cancellation") {
				t.Fatalf("%s without a durable cancellation: %v", action, err)
			}
			stored, _ := store.GetRun(ctx, owner.TenantID, run.ID)
			queued, _ := store.GetQueued(ctx, owner.TenantID, q.ID)
			requested, _ := store.RunCancelRequested(ctx, owner.TenantID, run.ID)
			if stored.Status != "waiting_external" || queued.Status != QueueStatusQueued || requested {
				t.Fatalf("half-cancelled work: run=%+v queue=%+v requested=%v", stored, queued, requested)
			}
			var outcomes int
			if err := store.db.QueryRow(`SELECT COUNT(*) FROM task_events WHERE run_id=? AND type='run.outcome'`, run.ID).Scan(&outcomes); err != nil || outcomes != 1 {
				t.Fatalf("failed cancellation left outcome history: %d,%v", outcomes, err)
			}
			checkpoint, err := store.IncompleteLoopCheckpointForRun(ctx, owner.TenantID, run.ID)
			if err != nil || checkpoint == nil {
				t.Fatalf("failed cancellation lost resumable checkpoint: %+v %v", checkpoint, err)
			}
		})
	}
}

func TestClearQueuedCancelsALeasedProviderWakeupBeforeItsChildStarts(t *testing.T) {
	store, owner, task, run, q := parkedProviderFixture(t)
	ctx := context.Background()
	token, claimed, err := store.ClaimQueued(ctx, owner.TenantID, q.ID, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claim=%v,%v", claimed, err)
	}
	if n, err := store.ClearQueued(ctx, owner.TenantID, owner.PersonID); err != nil || n != 1 {
		t.Fatalf("clear=%d,%v", n, err)
	}
	if child, err := store.StartRunWithOptions(ctx, task, run.Channel, "too late", StartRunOptions{ResumesRunID: run.ID, QueueID: q.ID, QueueClaimToken: token}); err == nil || child != nil {
		t.Fatalf("stale wakeup started a child: %+v,%v", child, err)
	}
	stored, _ := store.GetRun(ctx, owner.TenantID, run.ID)
	queued, _ := store.GetQueued(ctx, owner.TenantID, q.ID)
	if stored.Status != "cancelled" || queued.Status != QueueStatusCancelled || queued.ClaimToken != "" {
		t.Fatalf("leased work survived clear: %+v %+v", stored, queued)
	}
}

func TestParkedProviderCancellationDoesNotCrossOwnershipOrRewriteAStartedChild(t *testing.T) {
	store, owner, task, run, q := parkedProviderFixture(t)
	ctx := context.Background()
	other, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	if cancelled, err := store.CancelParkedProviderRun(ctx, other.TenantID, other.PersonID, run.ID); err != nil || cancelled {
		t.Fatalf("another person cancelled the wait: %v,%v", cancelled, err)
	}
	token, claimed, err := store.ClaimQueued(ctx, owner.TenantID, q.ID, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claim=%v,%v", claimed, err)
	}
	child, err := store.StartRunWithOptions(ctx, task, run.Channel, "already underway", StartRunOptions{ResumesRunID: run.ID, QueueID: q.ID, QueueClaimToken: token})
	if err != nil {
		t.Fatal(err)
	}
	if cancelled, err := store.CancelParkedProviderRun(ctx, owner.TenantID, owner.PersonID, run.ID); err != nil || cancelled {
		t.Fatalf("the claimed parent was rewritten: %v,%v", cancelled, err)
	}
	if n, err := store.ClearQueued(ctx, owner.TenantID, owner.PersonID); err != nil || n != 0 {
		t.Fatalf("clear cancelled live work: %d,%v", n, err)
	}
	stored, _ := store.GetRun(ctx, owner.TenantID, child.ID)
	queued, _ := store.GetQueued(ctx, owner.TenantID, q.ID)
	if stored.Status != "running" || queued.Status != QueueStatusStarted || queued.RunID != child.ID {
		t.Fatalf("live child lost ownership: %+v %+v", stored, queued)
	}
}

func TestConcurrentStopAndProviderWakeupChildCreationHaveOneDurableWinner(t *testing.T) {
	for range 12 {
		store, owner, task, run, q := parkedProviderFixture(t)
		other, err := OpenStore(store.dataDir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = other.Close() })
		ctx := context.Background()
		token, claimed, err := other.ClaimQueued(ctx, owner.TenantID, q.ID, time.Minute)
		if err != nil || !claimed {
			t.Fatalf("claim=%v,%v", claimed, err)
		}
		start := make(chan struct{})
		stops := make(chan bool, 1)
		children := make(chan *Run, 1)
		go func() {
			<-start
			cancelled, _ := store.CancelParkedProviderRun(ctx, owner.TenantID, owner.PersonID, run.ID)
			stops <- cancelled
		}()
		go func() {
			<-start
			child, _ := other.StartRunWithOptions(ctx, task, run.Channel, "race the stop", StartRunOptions{ResumesRunID: run.ID, QueueID: q.ID, QueueClaimToken: token})
			children <- child
		}()
		close(start)
		cancelled, child := <-stops, <-children
		if cancelled == (child != nil) {
			t.Fatalf("stop/launch have no single winner: cancelled=%v child=%+v", cancelled, child)
		}
		stored, _ := store.GetRun(ctx, owner.TenantID, run.ID)
		queued, _ := store.GetQueued(ctx, owner.TenantID, q.ID)
		if cancelled && (stored.Status != "cancelled" || queued.Status != QueueStatusCancelled) {
			t.Fatalf("stop winner is incomplete: %+v %+v", stored, queued)
		}
		if child != nil && (stored.Status != "waiting_external" || queued.RunID != child.ID || queued.Status != QueueStatusStarted) {
			t.Fatalf("launch winner is incomplete: %+v %+v", stored, queued)
		}
	}
}
