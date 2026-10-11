package control

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func parkedExecutionFixture(t *testing.T, host ...bool) (*Store, *IdentityContext, *Task, *Run, *QueuedTask) {
	t.Helper()
	store, owner, task, run := newRecoveryFixture(t)
	cp := LoopCheckpointRecord{TenantID: owner.TenantID, PersonID: owner.PersonID, TaskID: task.ID, RunID: run.ID, Outcome: "await_execution", WaitKind: LoopCheckpointWaitExecutionLane, WaitRef: run.ID, WaitToolCallID: "pending-write", Snapshot: []byte(`[{"role":"assistant","tool_calls":[{"id":"pending-write","function":"write_file","args":"{}"}]}]`)}
	if len(host) > 0 && host[0] {
		cp.WaitKind = LoopCheckpointWaitExecutionHostLane
	}
	if err := store.SaveLoopCheckpoint(context.Background(), cp); err != nil {
		t.Fatal(err)
	}
	q := &QueuedTask{TenantID: owner.TenantID, PersonID: owner.PersonID, TaskID: task.ID, ReplyToRunID: run.ID, Content: "continue pending call", Channel: run.Channel, Platform: "cli", PlatformUserID: "local", IdempotencyKey: "execution-lane:" + run.ID, Class: QueueClassFinalization, NotBefore: time.Now().Add(-time.Second)}
	if _, err := store.MaterializeRunFinalization(context.Background(), RunFinalization{Identity: *owner, RunID: run.ID, TaskID: task.ID, RunStatus: "waiting_external", ExpectedRunStatus: "running", RequireCheckpoint: true, Continuation: q}); err != nil {
		t.Fatal(err)
	}
	return store, owner, task, run, q
}

func TestExecutionWaitCancellationClosesParentAndQueueAtomically(t *testing.T) {
	for _, action := range []string{"stop", "clear", "drop"} {
		for _, failed := range []bool{false, true} {
			t.Run(action+map[bool]string{false: "", true: "/rollback"}[failed], func(t *testing.T) {
				store, owner, _, run, q := parkedExecutionFixture(t)
				if failed {
					if _, err := store.db.Exec(`CREATE TRIGGER fail_execution_cancel BEFORE INSERT ON task_events WHEN NEW.type='run.cancelled' BEGIN SELECT RAISE(ABORT,'injected cancellation failure'); END`); err != nil {
						t.Fatal(err)
					}
				}
				ctx := context.Background()
				var changed bool
				var err error
				switch action {
				case "stop":
					changed, err = store.CancelParkedProviderRun(ctx, owner.TenantID, owner.PersonID, run.ID)
				case "drop":
					changed, err = store.CancelQueued(ctx, owner.TenantID, owner.PersonID, q.ID)
				case "clear":
					var n int
					n, err = store.ClearQueued(ctx, owner.TenantID, owner.PersonID)
					changed = n == 1
				}
				parent, _ := store.GetRun(ctx, owner.TenantID, run.ID)
				queue, _ := store.GetQueued(ctx, owner.TenantID, q.ID)
				if failed {
					if err == nil || !strings.Contains(err.Error(), "injected cancellation failure") || changed || parent.Status != "waiting_external" || queue.Status != QueueStatusQueued {
						t.Fatalf("failed cancel lost wait: %v %+v %+v %v", changed, parent, queue, err)
					}
					return
				}
				if err != nil || !changed || parent.Status != "cancelled" || queue.Status != QueueStatusCancelled {
					t.Fatalf("cancel was partial: %v %+v %+v %v", changed, parent, queue, err)
				}
				var obsolete *QueuedContinuationObsolete
				if resolved, wait, err := store.ResolveQueuedContinuation(ctx, *q); !errors.As(err, &obsolete) || wait || resolved.ReplyToRunID != run.ID || obsolete.Status != "cancelled" {
					t.Fatalf("exact finalization was rebased: %+v %v", resolved, err)
				}
			})
		}
	}
}

func TestExecutionWaitRejectsWrongBindingAndPreservesClaimedChild(t *testing.T) {
	store, owner, task, run, q := parkedExecutionFixture(t)
	ctx := context.Background()
	for _, cp := range []LoopCheckpointRecord{
		{TenantID: owner.TenantID, PersonID: owner.PersonID, TaskID: task.ID, RunID: run.ID, Outcome: "await_execution", WaitKind: LoopCheckpointWaitExecutionLane, WaitRef: "other-run", WaitToolCallID: "pending"},
		{TenantID: owner.TenantID, PersonID: owner.PersonID, TaskID: task.ID, RunID: run.ID, Outcome: "await_human", WaitKind: LoopCheckpointWaitExecutionLane, WaitRef: run.ID, WaitToolCallID: "pending"},
	} {
		if err := store.SaveLoopCheckpoint(ctx, cp); err == nil {
			t.Fatalf("wrong binding saved: %+v", cp)
		}
	}
	if child, err := store.StartRunWithOptions(ctx, task, run.Channel, "manual steal", StartRunOptions{ResumesRunID: run.ID}); err == nil || child != nil {
		t.Fatalf("manual resume stole queued boundary: %+v %v", child, err)
	}
	token, claimed, err := store.ClaimQueued(ctx, owner.TenantID, q.ID, time.Minute)
	if err != nil || !claimed {
		t.Fatal(err)
	}
	child, err := store.StartRunWithOptions(ctx, task, run.Channel, "exact queue", StartRunOptions{ResumesRunID: run.ID, QueueID: q.ID, QueueClaimToken: token})
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := store.CancelParkedProviderRun(ctx, owner.TenantID, owner.PersonID, run.ID); err != nil || changed {
		t.Fatalf("rewrote claimed parent: %v %v", changed, err)
	}
	if n, err := store.ClearQueued(ctx, owner.TenantID, owner.PersonID); err != nil || n != 0 {
		t.Fatalf("clear rewrote live child: %d %v", n, err)
	}
	stored, _ := store.GetRun(ctx, owner.TenantID, child.ID)
	queue, _ := store.GetQueued(ctx, owner.TenantID, q.ID)
	if stored.Status != "running" || queue.RunID != child.ID || queue.Status != QueueStatusStarted {
		t.Fatalf("child state lost: %+v %+v", stored, queue)
	}
}

func TestQueuedHostExecutionWaitsDerivesOnlyPendingExactOwnedQueue(t *testing.T) {
	store, owner, task, run, q := parkedExecutionFixture(t, true)
	ctx := context.Background()
	requireRefs := func(want bool) {
		t.Helper()
		refs, err := store.QueuedHostExecutionWaits(ctx, owner.TenantID, owner.PersonID)
		if err != nil || (want && (len(refs) != 1 || refs[0] != run.ID)) || (!want && len(refs) != 0) {
			t.Fatalf("host queue refs=%v err=%v want=%v", refs, err, want)
		}
	}
	requireRefs(true)
	foreign, err := store.QueuedHostExecutionWaits(ctx, owner.TenantID, "another-person")
	if err != nil || len(foreign) != 0 {
		t.Fatalf("foreign host intent leaked: %v %v", foreign, err)
	}
	token, claimed, err := store.ClaimQueued(ctx, owner.TenantID, q.ID, time.Minute)
	if err != nil || !claimed {
		t.Fatal(err)
	}
	requireRefs(true)
	child, err := store.StartRunWithOptions(ctx, task, run.Channel, "host queue", StartRunOptions{ResumesRunID: run.ID, QueueID: q.ID, QueueClaimToken: token})
	if err != nil {
		t.Fatal(err)
	}
	requireRefs(true)
	if _, err := store.db.Exec(`UPDATE runs SET status='waiting_user' WHERE id=?`, child.ID); err != nil {
		t.Fatal(err)
	}
	requireRefs(false)
	// Ordinary or legacy untyped waits do not gain writer priority.
	if _, err := store.db.Exec(`UPDATE runs SET status='running' WHERE id=?`, child.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE loop_checkpoints SET wait_kind='execution_lane' WHERE run_id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	requireRefs(false)
}
