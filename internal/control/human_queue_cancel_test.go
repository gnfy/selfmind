package control

import (
	"context"
	"testing"
	"time"
)

func TestRemovingHumanContinuationClosesItsExactParkedParent(t *testing.T) {
	for _, kind := range []string{LoopCheckpointWaitApproval, LoopCheckpointWaitClarification} {
		for _, action := range []string{"drop", "clear"} {
			t.Run(kind+"/"+action, func(t *testing.T) {
				store, owner, task, run, ref := parkedHumanCancelFixture(t, kind)
				ctx := context.Background()
				q := answerHumanCancelFixture(t, store, owner, task, kind, ref)
				if action == "drop" {
					if dropped, err := store.CancelQueued(ctx, owner.TenantID, owner.PersonID, q.ID); err != nil || !dropped {
						t.Fatalf("drop=%v %v", dropped, err)
					}
				} else if count, err := store.ClearQueued(ctx, owner.TenantID, owner.PersonID); err != nil || count != 1 {
					t.Fatalf("clear=%d %v", count, err)
				}
				parent, _ := store.GetRun(ctx, owner.TenantID, run.ID)
				queued, _ := store.GetQueued(ctx, owner.TenantID, q.ID)
				if parent.Status != "cancelled" || queued.Status != QueueStatusCancelled {
					t.Fatalf("removed answer orphaned its parent: Run=%s queue=%s", parent.Status, queued.Status)
				}
				if kind == LoopCheckpointWaitApproval {
					approval, _ := store.GetApprovalRequest(ctx, owner.TenantID, ref)
					if approval.Status != "approved" || approval.AuthorizationState != "" {
						t.Fatalf("cancel did not preserve the decision and withdraw its one-shot: %+v", approval)
					}
				}
				roots, err := store.ListRunRootReservations(ctx, owner.TenantID, owner.PersonID)
				if err != nil || len(roots) != 0 {
					t.Fatalf("closed wait kept a reservation: %+v %v", roots, err)
				}
			})
		}
	}
}

func removeHumanQueue(ctx context.Context, store *Store, owner *IdentityContext, queueID, action string) (int, error) {
	if action == "clear" {
		return store.ClearQueued(ctx, owner.TenantID, owner.PersonID)
	}
	dropped, err := store.CancelQueued(ctx, owner.TenantID, owner.PersonID, queueID)
	if dropped {
		return 1, err
	}
	return 0, err
}

func TestRemovingHumanContinuationIncludesLeasedUnstartedAnswers(t *testing.T) {
	for _, kind := range []string{LoopCheckpointWaitApproval, LoopCheckpointWaitClarification} {
		for _, action := range []string{"drop", "clear"} {
			t.Run(kind+"/"+action, func(t *testing.T) {
				store, owner, task, run, ref := parkedHumanCancelFixture(t, kind)
				ctx := context.Background()
				q := answerHumanCancelFixture(t, store, owner, task, kind, ref)
				// Released historical queues may lack the later explicit reply edge.
				// The owned request and checkpoint still identify the exact parent.
				if _, err := store.db.Exec(`UPDATE task_queue SET reply_to_run_id='' WHERE id=?`, q.ID); err != nil {
					t.Fatal(err)
				}
				token, claimed, err := store.ClaimQueued(ctx, owner.TenantID, q.ID, time.Minute)
				if err != nil || !claimed {
					t.Fatalf("claim=%v %v", claimed, err)
				}
				if n, err := removeHumanQueue(ctx, store, owner, q.ID, action); err != nil || n != 1 {
					t.Fatalf("leased answer removal=%d %v", n, err)
				}
				parent, _ := store.GetRun(ctx, owner.TenantID, run.ID)
				queued, _ := store.GetQueued(ctx, owner.TenantID, q.ID)
				if parent.Status != "cancelled" || queued.Status != QueueStatusCancelled || queued.ClaimToken != "" {
					t.Fatalf("leased human answer survived removal: parent=%+v queue=%+v", parent, queued)
				}
				if child, err := store.StartRunWithOptions(ctx, task, run.Channel, "stale answer", StartRunOptions{ResumesRunID: run.ID, QueueID: q.ID, QueueClaimToken: token}); err == nil || child != nil {
					t.Fatalf("stale lease started cancelled work: %+v %v", child, err)
				}
			})
		}
	}
}

func TestRemovingHumanContinuationRollsBackWhenClosureCannotBeRecorded(t *testing.T) {
	for _, kind := range []string{LoopCheckpointWaitApproval, LoopCheckpointWaitClarification} {
		for _, action := range []string{"drop", "clear"} {
			t.Run(kind+"/"+action, func(t *testing.T) {
				store, owner, task, run, ref := parkedHumanCancelFixture(t, kind)
				ctx := context.Background()
				q := answerHumanCancelFixture(t, store, owner, task, kind, ref)
				if _, err := store.db.Exec(`CREATE TRIGGER reject_queue_human_stop BEFORE INSERT ON task_events WHEN NEW.type='run.cancelled'
				 BEGIN SELECT RAISE(ABORT,'injected human queue cancellation failure'); END`); err != nil {
					t.Fatal(err)
				}
				if n, err := removeHumanQueue(ctx, store, owner, q.ID, action); err == nil || n != 0 {
					t.Fatalf("failed closure dropped answered work: %d %v", n, err)
				}
				parent, _ := store.GetRun(ctx, owner.TenantID, run.ID)
				queued, _ := store.GetQueued(ctx, owner.TenantID, q.ID)
				roots, err := store.ListRunRootReservations(ctx, owner.TenantID, owner.PersonID)
				if parent.Status != "waiting_user" || queued.Status != QueueStatusQueued || err != nil || len(roots) != 0 {
					t.Fatalf("failed closure lost recoverable work: parent=%+v queue=%+v roots=%+v err=%v", parent, queued, roots, err)
				}
				if kind == LoopCheckpointWaitApproval {
					approval, _ := store.GetApprovalRequest(ctx, owner.TenantID, ref)
					if approval.AuthorizationState != "available" {
						t.Fatalf("failed closure withdrew the recorded decision: %+v", approval)
					}
				}
			})
		}
	}
}

func TestRemovingHumanContinuationLeavesStartedChildAndPendingSiblingAlone(t *testing.T) {
	for _, kind := range []string{LoopCheckpointWaitApproval, LoopCheckpointWaitClarification} {
		for _, action := range []string{"drop", "clear"} {
			t.Run(kind+"/"+action, func(t *testing.T) {
				store, owner, task, run, ref := parkedHumanCancelFixture(t, kind)
				ctx := context.Background()
				_, siblingTask, sibling := humanWaitRun(t, store, "human-cancel-owner")
				question := pendingClarification(t, store, owner, siblingTask, sibling)
				if err := store.SaveLoopCheckpoint(ctx, awaitCheckpoint(owner, siblingTask, sibling, LoopCheckpointWaitClarification, question.ID, "sibling-call")); err != nil {
					t.Fatal(err)
				}
				if parked, err := store.ParkHumanWaitRun(ctx, owner.TenantID, owner.PersonID, sibling.ID, LoopCheckpointWaitClarification, question.ID, HumanWaitParkGrace); err != nil || !parked {
					t.Fatalf("sibling park=%v %v", parked, err)
				}
				q := answerHumanCancelFixture(t, store, owner, task, kind, ref)
				token, claimed, err := store.ClaimQueued(ctx, owner.TenantID, q.ID, time.Minute)
				if err != nil || !claimed {
					t.Fatalf("claim=%v %v", claimed, err)
				}
				child, err := store.StartRunWithOptions(ctx, task, run.Channel, "already started", StartRunOptions{ResumesRunID: run.ID, QueueID: q.ID, QueueClaimToken: token})
				if err != nil {
					t.Fatal(err)
				}
				if n, err := removeHumanQueue(ctx, store, owner, q.ID, action); err != nil || n != 0 {
					t.Fatalf("removed a bound child: %d %v", n, err)
				}
				current, _ := store.GetRun(ctx, owner.TenantID, child.ID)
				pending, _ := store.GetRun(ctx, owner.TenantID, sibling.ID)
				ask, _ := store.GetClarifyRequest(ctx, owner.TenantID, question.ID)
				queued, _ := store.GetQueued(ctx, owner.TenantID, q.ID)
				if current.Status != "running" || queued.Status != QueueStatusStarted || queued.RunID != child.ID || pending.Status != "waiting_user" || ask.Status != "pending" {
					t.Fatalf("queue control crossed its edge: child=%+v queue=%+v sibling=%+v question=%+v", current, queued, pending, ask)
				}
			})
		}
	}
}
