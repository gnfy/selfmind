package control

import (
	"context"
	"strings"
	"testing"
)

func TestCancellationWinsHumanParkAndFinalization(t *testing.T) {
	for _, kind := range []string{LoopCheckpointWaitApproval, LoopCheckpointWaitClarification} {
		for _, parked := range []bool{false, true} {
			name := kind + "/live"
			if parked {
				name = kind + "/parked"
			}
			t.Run(name, func(t *testing.T) {
				store := newTestStore(t)
				owner, task, run := humanWaitRun(t, store, "cancel-boundary")
				ctx := context.Background()
				ref := ""
				if kind == LoopCheckpointWaitApproval {
					ref = pendingApproval(t, store, owner, task, run).ID
				} else {
					ref = pendingClarification(t, store, owner, task, run).ID
				}
				if err := store.SaveLoopCheckpoint(ctx, awaitCheckpoint(owner, task, run, kind, ref, "uninvoked")); err != nil {
					t.Fatal(err)
				}
				if parked {
					if changed, err := store.ParkHumanWaitRun(ctx, owner.TenantID, owner.PersonID, run.ID, kind, ref); err != nil || !changed {
						t.Fatalf("park=%v %v", changed, err)
					}
				}
				if err := store.RequestRunCancel(ctx, owner.TenantID, run.ID); err != nil {
					t.Fatal(err)
				}
				status := ""
				if kind == LoopCheckpointWaitApproval {
					ask, _ := store.GetApprovalRequest(ctx, owner.TenantID, ref)
					status = ask.Status
				} else {
					ask, _ := store.GetClarifyRequest(ctx, owner.TenantID, ref)
					status = ask.Status
				}
				if status != "expired" {
					t.Fatalf("cancel retained a pending %s: %s", kind, status)
				}
				current, _ := store.GetRun(ctx, owner.TenantID, run.ID)
				expected := "running"
				if parked {
					expected = "cancelled"
				}
				if current.Status != expected {
					t.Fatalf("unobserved live body or exact park has wrong status: %s want %s", current.Status, expected)
				}
				stale := RunFinalization{Identity: *owner, TaskID: task.ID, RunID: run.ID, RunStatus: "waiting_user", Event: Event{Payload: []byte(`{"outcome":{"status":"waiting_user"}}`)}}
				if _, err := store.MaterializeRunFinalization(ctx, stale); err == nil {
					t.Fatal("stale finalization overwrote the cancellation")
				}
				if !parked {
					if changed, err := store.ParkHumanWaitRun(ctx, owner.TenantID, owner.PersonID, run.ID, kind, ref); changed {
						t.Fatalf("cancelled call parked again: %v", err)
					}
					stale.RunStatus = "cancelled"
					stale.Event.Type = "run.cancelled"
					stale.Event.Payload = []byte(`{"outcome":{"status":"cancelled","resumable":false}}`)
					if _, err := store.MaterializeRunFinalization(ctx, stale); err != nil {
						t.Fatal(err)
					}
				}
				ids, err := store.ParkedHumanRunIDs(ctx, owner.TenantID, owner.PersonID)
				if err != nil || len(ids) != 0 {
					t.Fatalf("cancelled Run retained a human wait: %v %v", ids, err)
				}
			})
		}
	}
}

func TestCancellationRequestRollsBackHumanStateOnEventFailure(t *testing.T) {
	store := newTestStore(t)
	owner, task, run := humanWaitRun(t, store, "cancel-fault")
	ask := pendingApproval(t, store, owner, task, run)
	if err := store.PinSessionResumeSelection(context.Background(), owner.TenantID, owner.PersonID, "session", task.ID, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER reject_cancel_resolution BEFORE INSERT ON task_events WHEN NEW.type='approval.expired' BEGIN SELECT RAISE(ABORT,'injected expiry failure'); END`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	err := store.RequestRunCancel(ctx, owner.TenantID, run.ID)
	if err == nil || !strings.Contains(err.Error(), "injected expiry failure") {
		t.Fatalf("cancel did not expose storage failure: %v", err)
	}
	requested, _ := store.RunCancelRequested(ctx, owner.TenantID, run.ID)
	current, _ := store.GetApprovalRequest(ctx, owner.TenantID, ask.ID)
	selected, selectErr := store.SessionResumeSelection(ctx, owner.TenantID, owner.PersonID, "session")
	if selectErr != nil || selected == nil || selected.RunID != run.ID {
		t.Fatalf("failed cancellation withdrew selection: %+v %v", selected, selectErr)
	}
	if requested || current.Status != "pending" {
		t.Fatalf("failed cancel partially committed: requested=%v request=%+v", requested, current)
	}
}

func TestCancellationPreservesConsumedApprovalEvidence(t *testing.T) {
	store := newTestStore(t)
	owner, task, run := humanWaitRun(t, store, "cancel-consumed")
	used := pendingApproval(t, store, owner, task, run)
	if _, err := store.db.Exec(`UPDATE approval_requests SET status='approved',authorization_state='consumed' WHERE id=?`, used.ID); err != nil {
		t.Fatal(err)
	}
	pending := pendingApproval(t, store, owner, task, run)
	ctx := context.Background()
	if err := store.RequestRunCancel(ctx, owner.TenantID, run.ID); err != nil {
		t.Fatal(err)
	}
	prior, priorErr := store.GetApprovalRequest(ctx, owner.TenantID, used.ID)
	current, currentErr := store.GetApprovalRequest(ctx, owner.TenantID, pending.ID)
	if priorErr != nil || currentErr != nil || prior.Status != "approved" || prior.AuthorizationState != "consumed" || current.Status != "expired" {
		t.Fatalf("stop rewrote used authority: prior=%+v current=%+v errors=%v/%v", prior, current, priorErr, currentErr)
	}
}
