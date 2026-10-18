package control

import (
	"context"
	"testing"
)

func TestHumanWaitParkingRequiresDurableBindingAndRollsBack(t *testing.T) {
	for _, kind := range []string{LoopCheckpointWaitApproval, LoopCheckpointWaitClarification} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			store := newTestStore(t)
			owner, task, run := humanWaitRun(t, store, "park-owner")
			var ref string
			if kind == LoopCheckpointWaitApproval {
				ref = pendingApproval(t, store, owner, task, run).ID
			} else {
				ref = pendingClarification(t, store, owner, task, run).ID
			}
			if parked, err := store.ParkHumanWaitRun(ctx, owner.TenantID, owner.PersonID, run.ID, kind, ref, HumanWaitParkGrace); err == nil || parked {
				t.Fatal("parked without a checkpoint")
			}
			if err := store.SaveLoopCheckpoint(ctx, awaitCheckpoint(owner, task, run, kind, ref, "pending-call")); err != nil {
				t.Fatal(err)
			}
			if parked, err := store.ParkHumanWaitRun(ctx, owner.TenantID, "different-owner", run.ID, kind, ref, HumanWaitParkGrace); err == nil || parked {
				t.Fatal("another person parked the Run")
			}
			if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_human_run BEFORE UPDATE ON runs WHEN NEW.status='waiting_user' BEGIN SELECT RAISE(ABORT,'injected run write failure'); END`); err != nil {
				t.Fatal(err)
			}
			if parked, err := store.ParkHumanWaitRun(ctx, owner.TenantID, owner.PersonID, run.ID, kind, ref, HumanWaitParkGrace); err == nil || parked {
				t.Fatal("failed transaction reported parking")
			}
			current, _ := store.GetRun(ctx, owner.TenantID, run.ID)
			if current.Status != "running" {
				t.Fatalf("failed transaction changed Run: %+v", current)
			}
			if kind == LoopCheckpointWaitApproval {
				a, _ := store.GetApprovalRequest(ctx, owner.TenantID, ref)
				if a.WaiterState != "live" || a.ParkedAt != nil {
					t.Fatalf("failed transaction changed request: %+v", a)
				}
			} else {
				q, _ := store.GetClarifyRequest(ctx, owner.TenantID, ref)
				if q.ParkedAt != nil {
					t.Fatalf("failed transaction changed request: %+v", q)
				}
			}
			if _, err := store.db.ExecContext(ctx, `DROP TRIGGER fail_human_run`); err != nil {
				t.Fatal(err)
			}
			if parked, err := store.ParkHumanWaitRun(ctx, owner.TenantID, owner.PersonID, run.ID, kind, ref, HumanWaitParkGrace); err != nil || !parked {
				t.Fatalf("durable checkpoint failed to park: %v %v", parked, err)
			}
		})
	}
}
func TestHumanWaitAnswerWinsBeforeParking(t *testing.T) {
	for _, kind := range []string{LoopCheckpointWaitApproval, LoopCheckpointWaitClarification} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			store := newTestStore(t)
			owner, task, run := humanWaitRun(t, store, "answer-owner")
			var ref string
			if kind == LoopCheckpointWaitApproval {
				ref = pendingApproval(t, store, owner, task, run).ID
			} else {
				ref = pendingClarification(t, store, owner, task, run).ID
			}
			if err := store.SaveLoopCheckpoint(ctx, awaitCheckpoint(owner, task, run, kind, ref, "pending-call")); err != nil {
				t.Fatal(err)
			}
			if kind == LoopCheckpointWaitApproval {
				if _, err := store.RespondApprovalRequest(ctx, owner.TenantID, owner.PersonID, ref, "approved", "cli", ApprovalDecisionInput{}); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, q, err := store.AnswerClarifyRequestWithResume(ctx, owner.TenantID, owner.PersonID, ref, "staging", "cli", QueuedTask{}); err != nil || q != nil {
					t.Fatalf("live answer queued: %+v %v", q, err)
				}
			}
			if parked, err := store.ParkHumanWaitRun(ctx, owner.TenantID, owner.PersonID, run.ID, kind, ref, HumanWaitParkGrace); err != nil || parked {
				t.Fatalf("in-place answer was parked: %v %v", parked, err)
			}
			current, _ := store.GetRun(ctx, owner.TenantID, run.ID)
			if current.Status != "running" {
				t.Fatalf("answer lost its current turn: %+v", current)
			}
		})
	}
}
