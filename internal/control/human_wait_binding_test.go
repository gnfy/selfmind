package control

import (
	"context"
	"errors"
	"testing"
	"time"
)

func humanWaitRun(t *testing.T, store *Store, platformUserID string) (*IdentityContext, *Task, *Run) {
	t.Helper()
	owner, task := admissionOwnerAndThread(t, store, platformUserID)
	run, err := store.StartRunWithOptions(context.Background(), task, "cli", "deploy", StartRunOptions{Admission: verifyOnlyCronAdmission()})
	if err != nil {
		t.Fatal(err)
	}
	return owner, task, run
}

func pendingApproval(t *testing.T, store *Store, owner *IdentityContext, task *Task, run *Run) *ApprovalRequest {
	t.Helper()
	approval, err := store.CreateApprovalRequest(context.Background(), ApprovalRequest{TenantID: owner.TenantID,
		PersonID: owner.PersonID, TaskID: task.ID, RunID: run.ID, ActionType: "tool_call", AuthorizationFingerprint: "resume:v1:test"})
	if err != nil {
		t.Fatal(err)
	}
	return approval
}

func pendingClarification(t *testing.T, store *Store, owner *IdentityContext, task *Task, run *Run) *ClarifyRequest {
	t.Helper()
	clarify, err := store.CreateClarifyRequest(context.Background(), ClarifyRequest{TenantID: owner.TenantID,
		PersonID: owner.PersonID, TaskID: task.ID, RunID: run.ID, Question: "Which environment?", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	return clarify
}

func awaitCheckpoint(owner *IdentityContext, task *Task, run *Run, kind, ref, call string) LoopCheckpointRecord {
	return LoopCheckpointRecord{TenantID: owner.TenantID, PersonID: owner.PersonID, TaskID: task.ID, RunID: run.ID,
		Iteration: 4, Outcome: "await_human", Snapshot: []byte(`[{"role":"user","content":"deploy"}]`),
		WaitKind: kind, WaitRef: ref, WaitToolCallID: call}
}

func TestLoopCheckpointBindsItsExactHumanWait(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, task, run := humanWaitRun(t, store, "owner")
	approval := pendingApproval(t, store, owner, task, run)
	if err := store.SaveLoopCheckpoint(ctx, awaitCheckpoint(owner, task, run, LoopCheckpointWaitApproval, approval.ID, " call_deploy ")); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := store.IncompleteLoopCheckpointForRun(ctx, owner.TenantID, run.ID)
	if err != nil || checkpoint == nil || checkpoint.WaitKind != LoopCheckpointWaitApproval ||
		checkpoint.WaitRef != approval.ID || checkpoint.WaitToolCallID != "call_deploy" || checkpoint.Outcome != "await_human" {
		t.Fatalf("bound checkpoint = %+v err=%v", checkpoint, err)
	}

	// The checkpoint is overwrite-only: a later iteration without a wait
	// carries no stale binding.
	unbound := awaitCheckpoint(owner, task, run, "", "", "")
	unbound.Outcome, unbound.Iteration = "execute_tools", 5
	if err := store.SaveLoopCheckpoint(ctx, unbound); err != nil {
		t.Fatal(err)
	}
	checkpoint, err = store.IncompleteLoopCheckpointForRun(ctx, owner.TenantID, run.ID)
	if err != nil || checkpoint == nil || checkpoint.WaitKind != "" || checkpoint.WaitRef != "" || checkpoint.WaitToolCallID != "" {
		t.Fatalf("rewritten checkpoint kept a binding: %+v err=%v", checkpoint, err)
	}

	// The same binding names a clarification on another Run, and a completed
	// checkpoint reads its binding through the other reader.
	clarifyOwner, clarifyTask, clarifyRun := humanWaitRun(t, store, "clarifier")
	clarify := pendingClarification(t, store, clarifyOwner, clarifyTask, clarifyRun)
	completed := awaitCheckpoint(clarifyOwner, clarifyTask, clarifyRun, LoopCheckpointWaitClarification, clarify.ID, "call_ask")
	completed.Outcome = "complete_turn"
	if err := store.SaveLoopCheckpoint(ctx, completed); err != nil {
		t.Fatal(err)
	}
	read, err := store.CompletedLoopCheckpointForRun(ctx, clarifyOwner.TenantID, clarifyOwner.PersonID, clarifyRun.ID)
	if err != nil || read == nil || read.WaitKind != LoopCheckpointWaitClarification || read.WaitRef != clarify.ID || read.WaitToolCallID != "call_ask" {
		t.Fatalf("clarification binding = %+v err=%v", read, err)
	}
}

func TestLoopCheckpointRejectsAWaitThatIsNotThisRuns(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, task, run := humanWaitRun(t, store, "owner")
	otherOwner, otherTask, otherRun := humanWaitRun(t, store, "other")
	approval := pendingApproval(t, store, owner, task, run)
	clarify := pendingClarification(t, store, owner, task, run)
	foreign := pendingApproval(t, store, otherOwner, otherTask, otherRun)
	sameOwnerTask, err := store.CreateTask(ctx, TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "sibling", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	siblingRun, err := store.StartRunWithOptions(ctx, sameOwnerTask, "cli", "sibling", StartRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sibling := pendingApproval(t, store, owner, sameOwnerTask, siblingRun)
	before := awaitCheckpoint(owner, task, run, "", "", "")
	before.Outcome = "execute_tools"
	if err := store.SaveLoopCheckpoint(ctx, before); err != nil {
		t.Fatal(err)
	}
	for name, record := range map[string]LoopCheckpointRecord{
		"kind without request":         awaitCheckpoint(owner, task, run, LoopCheckpointWaitApproval, "", "call"),
		"request without tool call":    awaitCheckpoint(owner, task, run, LoopCheckpointWaitApproval, approval.ID, ""),
		"request without kind":         awaitCheckpoint(owner, task, run, "", approval.ID, "call"),
		"unknown kind":                 awaitCheckpoint(owner, task, run, "watcher", approval.ID, "call"),
		"clarification named approval": awaitCheckpoint(owner, task, run, LoopCheckpointWaitApproval, clarify.ID, "call"),
		"approval named clarification": awaitCheckpoint(owner, task, run, LoopCheckpointWaitClarification, approval.ID, "call"),
		"another person's approval":    awaitCheckpoint(owner, task, run, LoopCheckpointWaitApproval, foreign.ID, "call"),
		"another Run's approval":       awaitCheckpoint(owner, task, run, LoopCheckpointWaitApproval, sibling.ID, "call"),
		"missing approval":             awaitCheckpoint(owner, task, run, LoopCheckpointWaitApproval, "apr_missing", "call"),
		"another person claiming my run": func() LoopCheckpointRecord {
			r := awaitCheckpoint(owner, task, run, LoopCheckpointWaitApproval, approval.ID, "call")
			r.PersonID = otherOwner.PersonID
			return r
		}(),
	} {
		if err := store.SaveLoopCheckpoint(ctx, record); !errors.Is(err, ErrLoopCheckpointWaitBinding) {
			t.Fatalf("%s: save error = %v, want ErrLoopCheckpointWaitBinding", name, err)
		}
	}
	checkpoint, err := store.IncompleteLoopCheckpointForRun(ctx, owner.TenantID, run.ID)
	if err != nil || checkpoint == nil || checkpoint.Outcome != "execute_tools" || checkpoint.WaitRef != "" {
		t.Fatalf("rejected bindings overwrote the checkpoint: %+v err=%v", checkpoint, err)
	}
}

func TestClarificationParkTimeIsExposed(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, task, run := humanWaitRun(t, store, "owner")
	clarify := pendingClarification(t, store, owner, task, run)
	if clarify.ParkedAt != nil {
		t.Fatalf("new question is parked: %v", clarify.ParkedAt)
	}
	if got, err := store.GetClarifyRequest(ctx, owner.TenantID, clarify.ID); err != nil || got == nil || got.ParkedAt != nil {
		t.Fatalf("unparked question reads parked: %+v err=%v", got, err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE clarify_requests SET parked_at = 1790000000 WHERE id = ?`, clarify.ID); err != nil {
		t.Fatal(err)
	}
	want := time.Unix(1790000000, 0)
	got, err := store.GetClarifyRequest(ctx, owner.TenantID, clarify.ID)
	if err != nil || got == nil || got.ParkedAt == nil || !got.ParkedAt.Equal(want) {
		t.Fatalf("parked question = %+v err=%v", got, err)
	}
	listed, err := store.ListClarifyRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
	if err != nil || len(listed) != 1 || listed[0].ParkedAt == nil || !listed[0].ParkedAt.Equal(want) {
		t.Fatalf("listed question = %+v err=%v", listed, err)
	}
	escrow, err := store.ListPendingClarifiesForEscrow(ctx, time.Now().Add(time.Minute))
	if err != nil || len(escrow) != 1 || escrow[0].ParkedAt == nil || !escrow[0].ParkedAt.Equal(want) {
		t.Fatalf("escrow question = %+v err=%v", escrow, err)
	}
}

// Every continuation that answers a parked human wait can carry the snapshot
// copied from the parked Run, read through its exact parent.
func TestHumanWaitResumeRowsCarryTheParkedRunsAdmission(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, task, run := humanWaitRun(t, store, "owner")
	inherited, err := store.ExactParentAdmissionSnapshot(ctx, owner.TenantID, owner.PersonID, run.ID)
	if err != nil || inherited.IsZero() {
		t.Fatalf("parked run admission = %+v err=%v", inherited, err)
	}
	stored := func(queued *QueuedTask) AdmissionSnapshot {
		t.Helper()
		if queued == nil {
			t.Fatal("no continuation row")
		}
		row, err := store.GetQueued(ctx, owner.TenantID, queued.ID)
		if err != nil || row == nil {
			t.Fatalf("continuation row %s: %+v err=%v", queued.ID, row, err)
		}
		return row.Admission
	}

	parked := pendingApproval(t, store, owner, task, run)
	if _, ok, err := store.ParkApprovalRequest(ctx, owner.TenantID, parked.ID, "wait budget elapsed"); err != nil || !ok {
		t.Fatalf("park: %v %v", ok, err)
	}
	if err := store.FinishRun(ctx, owner.TenantID, run.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	clarify := pendingClarification(t, store, owner, task, run)

	// A malformed snapshot is refused before the answer is recorded.
	broken := inherited
	broken.RecoveryMode = "replay_everything"
	if _, _, err := store.RespondParkedApprovalAndEnqueue(ctx, owner.TenantID, owner.PersonID, parked.ID, "approved", "cli",
		ApprovalDecisionInput{DecisionID: "once"}, QueuedTask{TaskID: task.ID, Content: "approved", IdempotencyKey: "approval-resume:" + parked.ID, Admission: broken}); err == nil {
		t.Fatal("parked approval accepted a malformed admission")
	}
	if still, err := store.GetApprovalRequest(ctx, owner.TenantID, parked.ID); err != nil || still.Status != "pending" || still.WaiterState != "parked" {
		t.Fatalf("refused answer changed the approval: %+v err=%v", still, err)
	}

	_, approvalRow, err := store.RespondParkedApprovalAndEnqueue(ctx, owner.TenantID, owner.PersonID, parked.ID, "approved", "cli",
		ApprovalDecisionInput{DecisionID: "once"}, QueuedTask{TaskID: task.ID, Content: "approved", IdempotencyKey: "approval-resume:" + parked.ID, Admission: inherited})
	if err != nil {
		t.Fatal(err)
	}
	if got := stored(approvalRow); got != inherited {
		t.Fatalf("approval-resume admission = %+v, want %+v", got, inherited)
	}

	_, clarifyRow, err := store.AnswerClarifyRequestWithResume(ctx, owner.TenantID, owner.PersonID, clarify.ID, "staging", "cli",
		QueuedTask{Admission: inherited})
	if err != nil {
		t.Fatal(err)
	}
	if clarifyRow.IdempotencyKey != "clarify-resume:"+clarify.ID || stored(clarifyRow) != inherited {
		t.Fatalf("clarify-resume row = %+v admission=%+v", clarifyRow, stored(clarifyRow))
	}

	// Crash recovery of an answered live approval writes the same snapshot.
	recoveredOwner, recoveredTask, recoveredRun := humanWaitRun(t, store, "recovered")
	live := pendingApproval(t, store, recoveredOwner, recoveredTask, recoveredRun)
	if _, err := store.RespondApprovalRequest(ctx, recoveredOwner.TenantID, recoveredOwner.PersonID, live.ID, "approved", "cli", ApprovalDecisionInput{DecisionID: "once"}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, recoveredOwner.TenantID, recoveredRun.ID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	recovered, created, err := store.EnqueueRecoveredApprovalContinuation(ctx, live.ID, QueuedTask{TenantID: recoveredOwner.TenantID,
		PersonID: recoveredOwner.PersonID, TaskID: recoveredTask.ID, Content: "approved", IdempotencyKey: "approval-resume:" + live.ID, Admission: inherited})
	if err != nil || !created {
		t.Fatalf("recovered continuation: created=%v err=%v", created, err)
	}
	row, err := store.GetQueued(ctx, recoveredOwner.TenantID, recovered.ID)
	if err != nil || row == nil || row.Admission != inherited {
		t.Fatalf("recovered approval-resume row = %+v err=%v", row, err)
	}
}
