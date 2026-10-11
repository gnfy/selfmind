package control

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestHeldChoiceRetainsParkedParent(t *testing.T) {
	for _, kind := range []string{"provider", "human"} {
		t.Run(kind, func(t *testing.T) {
			store, owner, task, run := newRecoveryFixture(t)
			ctx := context.Background()
			choice, err := store.CreatePendingTurnChoice(ctx, PendingTurnChoiceCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Channel: "im", RequestJSON: `{"kind":"multi_active","content":"add this constraint"}`, Options: []TurnChoiceOption{{Key: "1", Label: "selected", Action: "steer", TaskID: task.ID, RunID: run.ID}, {Key: "2", Label: "new", Action: "new"}}, ExpiresAt: time.Now().Add(time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			if kind == "provider" {
				parkProviderFixtureRun(t, store, owner, task, run)
			} else {
				if err := store.FinishRun(ctx, owner.TenantID, run.ID, "waiting_user"); err != nil {
					t.Fatal(err)
				}
			}
			routed, err := store.RoutePendingTurnChoice(ctx, TurnChoiceWorkRoute{TenantID: owner.TenantID, PersonID: owner.PersonID, ChoiceID: choice.ID, OptionKey: "1", RequestJSON: choice.RequestJSON, Queued: QueuedTask{TenantID: owner.TenantID, PersonID: owner.PersonID, Platform: "weixin", PlatformUserID: "wx", Channel: "im", Content: "add this constraint"}, Steering: SteeringMessage{TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: run.ID, TaskID: task.ID, Channel: "im", Content: "add this constraint"}})
			if err != nil {
				t.Fatal(err)
			}
			if routed.Queued == nil || routed.Queued.ReplyToRunID != run.ID {
				t.Fatalf("choice supplement lost exact parked parent %s: %+v", run.ID, routed.Queued)
			}
		})
	}
}

func TestArchivedHumanWaitStillCanBeStopped(t *testing.T) {
	for _, age := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "archived"}[age], func(t *testing.T) {
			store, owner, _, run, ref := parkedHumanCancelFixture(t, LoopCheckpointWaitApproval)
			ctx := context.Background()
			if age {
				if _, err := store.db.Exec(`UPDATE approval_requests SET parked_at=? WHERE id=?`, time.Now().Add(-8*24*time.Hour).Unix(), ref); err != nil {
					t.Fatal(err)
				}
				rows, err := store.ArchiveStaleParkedApprovals(ctx, 7*24*time.Hour)
				if err != nil || len(rows) != 1 {
					t.Fatalf("archive %+v %v", rows, err)
				}
			}
			cancelled, err := store.CancelParkedHumanRun(ctx, owner.TenantID, owner.PersonID, run.ID)
			if err != nil || !cancelled {
				t.Fatalf("owned exact await_human run no longer cancellable: cancelled=%v err=%v", cancelled, err)
			}

			if age {
				ask, err := store.GetApprovalRequest(ctx, owner.TenantID, ref)
				if err != nil || ask.Status != "archived" || ask.AuthorizationState != "" {
					t.Fatalf("cancel rewrote archived decision: %+v %v", ask, err)
				}
				if _, err := store.RespondApprovalRequest(ctx, owner.TenantID, owner.PersonID, ref, "approved", "cli", ApprovalDecisionInput{}); err == nil {
					t.Fatal("archived cancellation allowed a late approval")
				}
			}
		})
	}
}
func TestDroppingExternalResourceWakeClosesParent(t *testing.T) {
	for _, action := range []string{"drop", "clear"} {
		t.Run(action, func(t *testing.T) {
			store, owner, waiter, q := resourceWakeCancelFixture(t)
			ctx := context.Background()
			if action == "drop" {
				if removed, err := store.CancelQueued(ctx, owner.TenantID, owner.PersonID, q.ID); err != nil || !removed {
					t.Fatalf("drop=%v %v", removed, err)
				}
			} else {
				if removed, err := store.ClearQueued(ctx, owner.TenantID, owner.PersonID); err != nil || removed != 1 {
					t.Fatalf("clear=%d %v", removed, err)
				}
			}
			parent, err := store.GetRun(ctx, owner.TenantID, waiter.ID)
			if err != nil || parent.Status != "cancelled" {
				t.Fatalf("removed resource wake orphaned parent: status=%s err=%v", parent.Status, err)
			}
			if got := effectDisposition(t, store, owner, waiter, "unresolved"); got != ExternalClaimLeftUncertain {
				t.Fatalf("cancellation released an unobserved effect: %s", got)
			}
			waits, err := store.ListExternalResourceWaits(ctx, owner.TenantID, owner.PersonID, 100)
			if err != nil || len(waits) != 0 {
				t.Fatalf("cancelled parent remains wakeable: %+v %v", waits, err)
			}

		})
	}
}

func resourceWakeCancelFixture(t *testing.T) (*Store, *IdentityContext, *Run, *QueuedTask) {
	t.Helper()
	store := newTestStore(t)
	ctx := context.Background()
	owner, waiter, busy, _ := externalEffectRuns(t, store)
	claimFor(t, store, owner, busy, "busy", "service:alpha", false)
	decision, err := store.ClaimExternalEffects(ctx, ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: waiter.ID, EffectID: "pending", TargetKeys: []string{"service:alpha"}})
	if err != nil || decision.Granted {
		t.Fatalf("claim %+v %v", decision, err)
	}
	claimFor(t, store, owner, waiter, "unresolved", "service:other", true)
	if err := store.FinishRun(ctx, owner.TenantID, waiter.ID, "waiting_external"); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveExternalEffectGroup(ctx, owner.TenantID, owner.PersonID, busy.ID, "busy", "observer:completed"); err != nil {
		t.Fatal(err)
	}
	wait := parkedWait(t, store, owner, waiter)
	if !wait.Ready {
		t.Fatalf("wait was not ready before production-equivalent enqueue: %+v", wait)
	}
	if err := store.MarkExternalResourceWaitQueued(ctx, wait.ExternalResourceWait); err != nil {
		t.Fatal(err)
	}
	q, err := store.EnqueueQueued(ctx, QueuedTask{TenantID: owner.TenantID, PersonID: owner.PersonID, TaskID: waiter.TaskID, ReplyToRunID: waiter.ID, Class: QueueClassFinalization, Content: "Continue the exact external-resource wait", IdempotencyKey: "external-resource:" + waiter.ID + ":pending"})
	if err != nil {
		t.Fatal(err)
	}
	return store, owner, waiter, q
}

func TestResourceWakeCancellationRequiresExactUnstartedOwnedWait(t *testing.T) {
	for _, constraint := range []string{"leased", "foreign person", "wrong effect", "bound child", "event failure"} {
		t.Run(constraint, func(t *testing.T) {
			store, owner, parent, q := resourceWakeCancelFixture(t)
			ctx := context.Background()
			personID := owner.PersonID
			wantRemoved, wantCancelled := true, false
			switch constraint {
			case "leased":
				if _, err := store.db.Exec(`UPDATE task_queue SET status='started',claim_token='leased',lease_until=? WHERE id=?`, time.Now().Add(time.Minute).Unix(), q.ID); err != nil {
					t.Fatal(err)
				}
				wantCancelled = true
			case "foreign person":
				personID, wantRemoved = "other-person", false
			case "wrong effect":
				if _, err := store.db.Exec(`UPDATE task_queue SET idempotency_key=? WHERE id=?`, "external-resource:"+parent.ID+":different", q.ID); err != nil {
					t.Fatal(err)
				}
			case "bound child":
				if _, err := store.db.Exec(`UPDATE task_queue SET status='started',run_id=? WHERE id=?`, parent.ID, q.ID); err != nil {
					t.Fatal(err)
				}
				wantRemoved = false
			case "event failure":
				if _, err := store.db.Exec(`CREATE TRIGGER reject_resource_cancel BEFORE INSERT ON task_events WHEN NEW.type='run.cancelled' BEGIN SELECT RAISE(ABORT,'resource cancel failure'); END`); err != nil {
					t.Fatal(err)
				}
				wantRemoved = false
			}
			removed, err := store.CancelQueued(ctx, owner.TenantID, personID, q.ID)
			if constraint == "event failure" {
				if err == nil || !strings.Contains(err.Error(), "resource cancel failure") {
					t.Fatalf("missing durable failure: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			run, runErr := store.GetRun(ctx, owner.TenantID, parent.ID)
			queued, queueErr := store.GetQueued(ctx, owner.TenantID, q.ID)
			if runErr != nil || queueErr != nil || removed != wantRemoved || (run.Status == "cancelled") != wantCancelled || (queued.Status == QueueStatusCancelled) != wantRemoved {
				t.Fatalf("cancellation crossed its boundary: removed=%v run=%+v queue=%+v errors=%v/%v", removed, run, queued, runErr, queueErr)
			}
		})
	}
}
