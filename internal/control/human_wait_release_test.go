package control

import (
	"context"
	"testing"
	"time"
)

// armWait does what a live waiter does when it starts: remember the checkpoint
// the loop last saved, then overwrite it with the exact pending-call binding.
func armWait(t *testing.T, store *Store, owner *IdentityContext, task *Task, run *Run, kind, ref string) *LoopCheckpointRecord {
	t.Helper()
	ctx := context.Background()
	if err := store.SaveLoopCheckpoint(ctx, LoopCheckpointRecord{TenantID: owner.TenantID, PersonID: owner.PersonID, TaskID: task.ID,
		RunID: run.ID, Iteration: 3, Outcome: "continue_model", Detail: "provider_request",
		Snapshot: []byte(`[{"role":"user","content":"before the model call"}]`)}); err != nil {
		t.Fatal(err)
	}
	prior, err := store.IncompleteLoopCheckpointForRun(ctx, owner.TenantID, run.ID)
	if err != nil || prior == nil {
		t.Fatalf("prior checkpoint = %+v err=%v", prior, err)
	}
	if err := store.SaveLoopCheckpoint(ctx, awaitCheckpoint(owner, task, run, kind, ref, "pending-call")); err != nil {
		t.Fatal(err)
	}
	return prior
}

func currentCheckpoint(t *testing.T, store *Store, owner *IdentityContext, run *Run) *LoopCheckpointRecord {
	t.Helper()
	checkpoint, err := store.IncompleteLoopCheckpointForRun(context.Background(), owner.TenantID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return checkpoint
}

func requireRestored(t *testing.T, got, prior *LoopCheckpointRecord) {
	t.Helper()
	if got == nil || got.Outcome != prior.Outcome || got.Detail != prior.Detail || got.Iteration != prior.Iteration ||
		string(got.Snapshot) != string(prior.Snapshot) || got.WaitKind != "" || got.WaitRef != "" || got.WaitToolCallID != "" {
		t.Fatalf("checkpoint = %+v, want the prior one %+v with no wait binding", got, prior)
	}
}

func TestParkHumanWaitRunRecordsWhyItParked(t *testing.T) {
	for _, tc := range []struct{ given, want string }{
		{HumanWaitParkCapacity, "capacity"}, {HumanWaitParkHoldCap, "hold_cap"}, {HumanWaitParkDraining, "draining"},
		{HumanWaitParkClientDetached, "client_detached"}, {"  capacity  ", "capacity"}, {"", "grace"},
	} {
		t.Run(tc.want+"/"+tc.given, func(t *testing.T) {
			ctx := context.Background()
			store := newTestStore(t)
			owner, task, run := humanWaitRun(t, store, "why-owner")
			approval := pendingApproval(t, store, owner, task, run)
			if err := store.SaveLoopCheckpoint(ctx, awaitCheckpoint(owner, task, run, LoopCheckpointWaitApproval, approval.ID, "pending-call")); err != nil {
				t.Fatal(err)
			}
			if parked, err := store.ParkHumanWaitRun(ctx, owner.TenantID, owner.PersonID, run.ID, LoopCheckpointWaitApproval, approval.ID, tc.given); err != nil || !parked {
				t.Fatalf("park = %v %v", parked, err)
			}
			current, _ := store.GetApprovalRequest(ctx, owner.TenantID, approval.ID)
			if current.WaiterState != "parked" || current.ParkReason != tc.want {
				t.Fatalf("approval = %s/%q, want parked/%q", current.WaiterState, current.ParkReason, tc.want)
			}
		})
	}
}

func TestClaimApprovalDecisionReleasingRestoresTheCheckpointTheWaitOverwrote(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, task, run := humanWaitRun(t, store, "release-owner")
	approval := pendingApproval(t, store, owner, task, run)
	prior := armWait(t, store, owner, task, run, LoopCheckpointWaitApproval, approval.ID)
	if current := currentCheckpoint(t, store, owner, run); current.Outcome != "await_human" || current.WaitRef != approval.ID {
		t.Fatalf("wait was not armed: %+v", current)
	}
	if _, err := store.RespondApprovalRequest(ctx, owner.TenantID, owner.PersonID, approval.ID, "approved", "cli", ApprovalDecisionInput{}); err != nil {
		t.Fatal(err)
	}
	claimed, transitioned, err := store.ClaimApprovalDecisionReleasing(ctx, owner.TenantID, approval.ID, run.ID, &WaitRelease{Prior: prior})
	if err != nil || !transitioned || claimed.WaiterState != "claimed" || claimed.ClaimedByRunID != run.ID {
		t.Fatalf("claim = %+v transitioned=%v err=%v", claimed, transitioned, err)
	}
	requireRestored(t, currentCheckpoint(t, store, owner, run), prior)

	// The claim is once-only, and so is the restore: a repeat changes nothing.
	later := armWait(t, store, owner, task, run, LoopCheckpointWaitApproval, approval.ID)
	if _, transitioned, err := store.ClaimApprovalDecisionReleasing(ctx, owner.TenantID, approval.ID, run.ID, &WaitRelease{Prior: later}); err != nil || transitioned {
		t.Fatalf("second claim transitioned=%v err=%v", transitioned, err)
	}
	if current := currentCheckpoint(t, store, owner, run); current.Outcome != "await_human" {
		t.Fatalf("a repeated claim rewrote the checkpoint: %+v", current)
	}
}

func TestClaimApprovalDecisionReleasingWithoutAPriorRemovesTheBinding(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, task, run := humanWaitRun(t, store, "release-none-owner")
	approval := pendingApproval(t, store, owner, task, run)
	if err := store.SaveLoopCheckpoint(ctx, awaitCheckpoint(owner, task, run, LoopCheckpointWaitApproval, approval.ID, "pending-call")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RespondApprovalRequest(ctx, owner.TenantID, owner.PersonID, approval.ID, "rejected", "cli", ApprovalDecisionInput{}); err != nil {
		t.Fatal(err)
	}
	if _, transitioned, err := store.ClaimApprovalDecisionReleasing(ctx, owner.TenantID, approval.ID, run.ID, &WaitRelease{}); err != nil || !transitioned {
		t.Fatalf("claim transitioned=%v err=%v", transitioned, err)
	}
	if current := currentCheckpoint(t, store, owner, run); current != nil {
		t.Fatalf("binding survived its consumption: %+v", current)
	}
}

func TestClaimApprovalDecisionReleasingIsAtomic(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, task, run := humanWaitRun(t, store, "atomic-owner")
	approval := pendingApproval(t, store, owner, task, run)
	prior := armWait(t, store, owner, task, run, LoopCheckpointWaitApproval, approval.ID)
	if _, err := store.RespondApprovalRequest(ctx, owner.TenantID, owner.PersonID, approval.ID, "approved", "cli", ApprovalDecisionInput{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_release BEFORE UPDATE ON loop_checkpoints BEGIN SELECT RAISE(ABORT,'injected checkpoint write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, transitioned, err := store.ClaimApprovalDecisionReleasing(ctx, owner.TenantID, approval.ID, run.ID, &WaitRelease{Prior: prior}); err == nil || transitioned {
		t.Fatalf("claim survived a failed release: transitioned=%v err=%v", transitioned, err)
	}
	current, _ := store.GetApprovalRequest(ctx, owner.TenantID, approval.ID)
	if current.WaiterState != "live" || current.ClaimedByRunID != "" {
		t.Fatalf("a failed release left the decision claimed: %+v", current)
	}
	if got := currentCheckpoint(t, store, owner, run); got.Outcome != "await_human" {
		t.Fatalf("a failed release changed the checkpoint: %+v", got)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER fail_release`); err != nil {
		t.Fatal(err)
	}
	if _, transitioned, err := store.ClaimApprovalDecisionReleasing(ctx, owner.TenantID, approval.ID, run.ID, &WaitRelease{Prior: prior}); err != nil || !transitioned {
		t.Fatalf("retry transitioned=%v err=%v", transitioned, err)
	}
	requireRestored(t, currentCheckpoint(t, store, owner, run), prior)
}

func TestClaimApprovalDecisionReleasingLeavesAnotherRequestsBindingAlone(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, task, run := humanWaitRun(t, store, "other-binding-owner")
	answered := pendingApproval(t, store, owner, task, run)
	waiting := pendingApproval(t, store, owner, task, run)
	prior := armWait(t, store, owner, task, run, LoopCheckpointWaitApproval, waiting.ID)
	if _, err := store.RespondApprovalRequest(ctx, owner.TenantID, owner.PersonID, answered.ID, "approved", "cli", ApprovalDecisionInput{}); err != nil {
		t.Fatal(err)
	}
	if _, transitioned, err := store.ClaimApprovalDecisionReleasing(ctx, owner.TenantID, answered.ID, run.ID, &WaitRelease{Prior: prior}); err != nil || !transitioned {
		t.Fatalf("claim transitioned=%v err=%v", transitioned, err)
	}
	if current := currentCheckpoint(t, store, owner, run); current.Outcome != "await_human" || current.WaitRef != waiting.ID {
		t.Fatalf("claiming one request released the binding of another: %+v", current)
	}
}

func TestReleaseHumanWaitOnlyActsOnALiveWait(t *testing.T) {
	for _, kind := range []string{LoopCheckpointWaitApproval, LoopCheckpointWaitClarification} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			store := newTestStore(t)
			owner, task, run := humanWaitRun(t, store, "live-wait-owner")
			var ref string
			if kind == LoopCheckpointWaitApproval {
				ref = pendingApproval(t, store, owner, task, run).ID
			} else {
				ref = pendingClarification(t, store, owner, task, run).ID
			}
			prior := armWait(t, store, owner, task, run, kind, ref)
			if err := store.ReleaseHumanWait(ctx, owner.TenantID, "someone-else", run.ID, kind, ref, WaitRelease{Prior: prior}); err != nil {
				t.Fatal(err)
			}
			if current := currentCheckpoint(t, store, owner, run); current.Outcome != "await_human" {
				t.Fatalf("another person released the wait: %+v", current)
			}
			if err := store.ReleaseHumanWait(ctx, owner.TenantID, owner.PersonID, run.ID, kind, ref, WaitRelease{Prior: prior}); err != nil {
				t.Fatal(err)
			}
			requireRestored(t, currentCheckpoint(t, store, owner, run), prior)
			// Releasing again, with nothing bound, is harmless.
			if err := store.ReleaseHumanWait(ctx, owner.TenantID, owner.PersonID, run.ID, kind, ref, WaitRelease{Prior: prior}); err != nil {
				t.Fatal(err)
			}
			requireRestored(t, currentCheckpoint(t, store, owner, run), prior)

			// A wait that parked keeps the exact checkpoint its answer resumes.
			if err := store.SaveLoopCheckpoint(ctx, awaitCheckpoint(owner, task, run, kind, ref, "pending-call")); err != nil {
				t.Fatal(err)
			}
			if parked, err := store.ParkHumanWaitRun(ctx, owner.TenantID, owner.PersonID, run.ID, kind, ref, HumanWaitParkCapacity); err != nil || !parked {
				t.Fatalf("park = %v %v", parked, err)
			}
			if err := store.ReleaseHumanWait(ctx, owner.TenantID, owner.PersonID, run.ID, kind, ref, WaitRelease{Prior: prior}); err != nil {
				t.Fatal(err)
			}
			if current := currentCheckpoint(t, store, owner, run); current.Outcome != "await_human" || current.WaitRef != ref {
				t.Fatalf("releasing a parked wait destroyed its checkpoint: %+v", current)
			}
			if err := store.ReleaseHumanWait(ctx, owner.TenantID, owner.PersonID, run.ID, kind, "", WaitRelease{}); err == nil {
				t.Fatal("a release without its request was accepted")
			}
		})
	}
}

// A stop finalizes the Run without waiting for its wait's handler to unwind, so
// the handler's release can arrive after the Run ended. The wait it wrote is then
// garbage whatever way the Run ended, and the release must still clear it.
func TestReleaseHumanWaitStillClearsTheBindingOfARunThatEndedFirst(t *testing.T) {
	for _, status := range []string{"cancelled", "interrupted", "failed", "done"} {
		for _, kind := range []string{LoopCheckpointWaitApproval, LoopCheckpointWaitClarification} {
			t.Run(status+"/"+kind, func(t *testing.T) {
				ctx := context.Background()
				store := newTestStore(t)
				owner, task, run := humanWaitRun(t, store, "ended-first-owner")
				var ref string
				if kind == LoopCheckpointWaitApproval {
					ref = pendingApproval(t, store, owner, task, run).ID
				} else {
					ref = pendingClarification(t, store, owner, task, run).ID
				}
				prior := armWait(t, store, owner, task, run, kind, ref)
				if err := store.FinishRun(ctx, owner.TenantID, run.ID, status); err != nil {
					t.Fatal(err)
				}
				if err := store.ReleaseHumanWait(ctx, owner.TenantID, owner.PersonID, run.ID, kind, ref, WaitRelease{Prior: prior}); err != nil {
					t.Fatal(err)
				}
				requireRestored(t, currentCheckpoint(t, store, owner, run), prior)

				// With nothing to put back, the binding is removed instead.
				if err := store.SaveLoopCheckpoint(ctx, awaitCheckpoint(owner, task, run, kind, ref, "pending-call")); err != nil {
					t.Fatal(err)
				}
				if err := store.ReleaseHumanWait(ctx, owner.TenantID, owner.PersonID, run.ID, kind, ref, WaitRelease{}); err != nil {
					t.Fatal(err)
				}
				if current := currentCheckpoint(t, store, owner, run); current != nil {
					t.Fatalf("the binding of an ended Run survived its release: %+v", current)
				}
			})
		}
	}
}

func TestHasDueQueuedWork(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, _, _ := humanWaitRun(t, store, "queue-owner")
	stranger, _ := admissionOwnerAndThread(t, store, "queue-stranger")
	has := func(who *IdentityContext) bool {
		t.Helper()
		got, err := store.HasDueQueuedWork(ctx, who.TenantID, who.PersonID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if has(owner) {
		t.Fatal("an empty queue reported work")
	}
	if _, err := store.HasDueQueuedWork(ctx, owner.TenantID, " "); err == nil {
		t.Fatal("a blank person was accepted")
	}
	delayed, err := store.EnqueueQueued(ctx, QueuedTask{TenantID: owner.TenantID, PersonID: owner.PersonID, Content: "after the backoff", NotBefore: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if has(owner) {
		t.Fatal("a row whose not_before lies ahead counted as waiting for a slot")
	}
	due, err := store.EnqueueQueued(ctx, QueuedTask{TenantID: owner.TenantID, PersonID: owner.PersonID, Content: "needs a slot now"})
	if err != nil {
		t.Fatal(err)
	}
	if !has(owner) || has(stranger) {
		t.Fatalf("due row: owner=%v stranger=%v", has(owner), has(stranger))
	}
	token, claimed, err := store.ClaimQueued(ctx, owner.TenantID, due.ID, 0)
	if err != nil || !claimed || token == "" {
		t.Fatalf("claim = %q %v %v", token, claimed, err)
	}
	if !has(owner) {
		t.Fatal("a claimed row that has no Run yet is starting, and still wants the slot")
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE task_queue SET run_id='run_bound' WHERE id=?`, due.ID); err != nil {
		t.Fatal(err)
	}
	if has(owner) {
		t.Fatal("a started row already bound to its Run is that Run's record, not waiting work")
	}
	if err := store.MarkQueued(ctx, owner.TenantID, delayed.ID, QueueStatusCancelled); err != nil {
		t.Fatal(err)
	}
	if has(owner) {
		t.Fatal("a cancelled row counted")
	}
}
