package control

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestParkedApprovalAnswerQueuesContinuationAndOneShotAuthorization(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	approval, err := store.CreateApprovalRequest(ctx, ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		ActionType: "tool_call", AuthorizationFingerprint: "resume:v1:exact-action",
	})
	if err != nil {
		t.Fatal(err)
	}
	if parked, changed, err := store.ParkApprovalRequest(ctx, identity.TenantID, approval.ID, "resource budget elapsed"); err != nil || !changed || parked.WaiterState != "parked" {
		t.Fatalf("park: approval=%+v changed=%v err=%v", parked, changed, err)
	}

	resolved, queued, err := store.RespondParkedApprovalAndEnqueue(ctx,
		identity.TenantID, identity.PersonID, approval.ID, "approved", "cli",
		ApprovalDecisionInput{DecisionID: "once"}, QueuedTask{
			PersonID: identity.PersonID, Platform: "cli", Channel: "cli",
			Content: "resume the parked approval", TaskID: task.ID,
			IdempotencyKey: "approval-resume:" + approval.ID,
		})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Status != "approved" || resolved.WaiterState != "parked" || resolved.ResumeQueueID == "" || queued == nil || queued.ID != resolved.ResumeQueueID {
		t.Fatalf("resolved=%+v queued=%+v", resolved, queued)
	}
	if rows, err := store.ListQueued(ctx, identity.TenantID, identity.PersonID, QueueStatusQueued); err != nil || len(rows) != 1 || rows[0].TaskID != task.ID {
		t.Fatalf("queued rows=%+v err=%v", rows, err)
	}

	// A different regenerated action cannot consume the capability.
	if _, _, _, claimed, err := store.ClaimApprovalResumeAuthorization(ctx, identity.TenantID, identity.PersonID, run.ID, "resume:v1:different"); err != nil || claimed {
		t.Fatalf("mismatched action claimed=%v err=%v", claimed, err)
	}
	otherPerson, err := store.ResolveOrCreateAccount(ctx, identity.TenantID, "cli", "other-person", "Other Person")
	if err != nil {
		t.Fatal(err)
	}
	// An unrelated run must not consume it even with the exact fingerprint: the
	// claim is keyed on the run lineage, not on a shared work grouping.
	unrelated, err := store.StartRun(ctx, task, "cli", "unrelated work")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, tenant, person, run string
	}{
		{name: "tenant", tenant: "another-tenant", person: identity.PersonID, run: run.ID},
		{name: "person", tenant: identity.TenantID, person: otherPerson.PersonID, run: run.ID},
		{name: "unrelated run", tenant: identity.TenantID, person: identity.PersonID, run: unrelated.ID},
		{name: "unknown run", tenant: identity.TenantID, person: identity.PersonID, run: "run-does-not-exist"},
	} {
		if _, _, _, claimed, err := store.ClaimApprovalResumeAuthorization(ctx, tc.tenant, tc.person, tc.run, "resume:v1:exact-action"); err != nil || claimed {
			t.Fatalf("mismatched %s claimed=%v err=%v", tc.name, claimed, err)
		}
	}
	// The resuming run claims it: park the waiter, then start the run that
	// resumes it, which is the production shape of answering a parked approval.
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	resumer, err := store.StartRunWithOptions(ctx, task, "cli", "resume the parked approval", StartRunOptions{ResumesRunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	id, decisionID, _, claimed, err := store.ClaimApprovalResumeAuthorization(ctx, identity.TenantID, identity.PersonID, resumer.ID, "resume:v1:exact-action")
	if err != nil || !claimed || id != approval.ID || decisionID != "once" {
		t.Fatalf("exact claim id=%q decision=%q claimed=%v err=%v", id, decisionID, claimed, err)
	}
	if _, _, _, claimed, err := store.ClaimApprovalResumeAuthorization(ctx, identity.TenantID, identity.PersonID, resumer.ID, "resume:v1:exact-action"); err != nil || claimed {
		t.Fatalf("one-shot capability reused: claimed=%v err=%v", claimed, err)
	}
}

func TestParkedApprovalResumeAuthorizationHasOneConcurrentWinner(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	approval, err := store.CreateApprovalRequest(ctx, ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		ActionType: "tool_call", AuthorizationFingerprint: "resume:v1:concurrent-action",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, changed, err := store.ParkApprovalRequest(ctx, identity.TenantID, approval.ID, "resource budget elapsed"); err != nil || !changed {
		t.Fatalf("park changed=%v err=%v", changed, err)
	}
	if _, _, err := store.RespondParkedApprovalAndEnqueue(ctx,
		identity.TenantID, identity.PersonID, approval.ID, "approved", "cli",
		ApprovalDecisionInput{DecisionID: "once"}, QueuedTask{
			PersonID: identity.PersonID, Platform: "cli", Channel: "cli",
			Content: "resume concurrent approval", TaskID: task.ID,
			IdempotencyKey: "approval-resume:" + approval.ID,
		}); err != nil {
		t.Fatal(err)
	}

	// One resuming run, several racing claimants: parallel tool calls in the
	// same run can regenerate the same action, and exactly one may consume the
	// one-shot capability. Distinct contender ids would not model this any more
	// — only one run may resume a given run.
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	resumer, err := store.StartRunWithOptions(ctx, task, "cli", "resume concurrent approval", StartRunOptions{ResumesRunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}

	const contenders = 8
	var wg sync.WaitGroup
	winners := make(chan string, contenders)
	errors := make(chan error, contenders)
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id, _, _, claimed, claimErr := store.ClaimApprovalResumeAuthorization(ctx,
				identity.TenantID, identity.PersonID, resumer.ID, "resume:v1:concurrent-action")
			if claimErr != nil {
				errors <- claimErr
				return
			}
			if claimed {
				winners <- id
			}
		}(i)
	}
	wg.Wait()
	close(winners)
	close(errors)
	for claimErr := range errors {
		t.Fatalf("concurrent claim: %v", claimErr)
	}
	var won []string
	for id := range winners {
		won = append(won, id)
	}
	if len(won) != 1 || won[0] != approval.ID {
		t.Fatalf("concurrent winners=%v, want exactly %s", won, approval.ID)
	}
}

func TestParkedApprovalRetentionArchivesWithoutInventingRejection(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	approval, err := store.CreateApprovalRequest(ctx, ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		ActionType: "tool_call",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, changed, err := store.ParkApprovalRequest(ctx, identity.TenantID, approval.ID, "resource budget elapsed"); err != nil || !changed {
		t.Fatalf("park changed=%v err=%v", changed, err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE approval_requests SET parked_at = ? WHERE id = ?`, time.Now().Add(-8*24*time.Hour).Unix(), approval.ID); err != nil {
		t.Fatal(err)
	}
	archived, err := store.ArchiveStaleParkedApprovals(ctx, 7*24*time.Hour)
	if err != nil || len(archived) != 1 || archived[0].ID != approval.ID || archived[0].Status != "archived" {
		t.Fatalf("archived=%+v err=%v", archived, err)
	}
	if archived[0].DecisionID != "" || archived[0].ApprovedChannel != "" {
		t.Fatalf("retention invented a human decision: %+v", archived[0])
	}
}

func TestApprovalBacklogSeparatesLiveAndParkedStock(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	live, err := store.CreateApprovalRequest(ctx, ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		ActionType: "tool_call",
	})
	if err != nil {
		t.Fatal(err)
	}
	parked, err := store.CreateApprovalRequest(ctx, ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		ActionType: "tool_call",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, changed, err := store.ParkApprovalRequest(ctx, identity.TenantID, parked.ID, "resource budget elapsed"); err != nil || !changed {
		t.Fatalf("park changed=%v err=%v", changed, err)
	}
	oldest := time.Now().Add(-3 * time.Hour).Unix()
	if _, err := store.db.ExecContext(ctx, `UPDATE approval_requests SET parked_at = ? WHERE id = ?`, oldest, parked.ID); err != nil {
		t.Fatal(err)
	}
	stats, err := store.ApprovalBacklog(ctx, identity.TenantID, identity.PersonID)
	if err != nil || stats.Live != 1 || stats.Parked != 1 || stats.OldestParkedAt == nil || stats.OldestParkedAt.Unix() != oldest {
		t.Fatalf("backlog=%+v err=%v live=%s", stats, err, live.ID)
	}
}

func TestInterruptedRunDecisionRecoveryIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	approval, err := store.CreateApprovalRequest(ctx, ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		ActionType: "tool_call", AuthorizationFingerprint: "resume:v1:crash-window",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RespondApprovalRequest(ctx, identity.TenantID, identity.PersonID, approval.ID, "approved", "cli", ApprovalDecisionInput{DecisionID: "once"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkInterruptedRuns(ctx, 0); err != nil {
		t.Fatal(err)
	}
	recoverable, err := store.ListRecoverableApprovalDecisions(ctx, 10)
	if err != nil || len(recoverable) != 1 || recoverable[0].ID != approval.ID {
		t.Fatalf("recoverable=%+v err=%v", recoverable, err)
	}

	input := QueuedTask{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Platform: "cli", Channel: "cli",
		Content: "recover committed decision", TaskID: task.ID,
		IdempotencyKey: "approval-resume:" + approval.ID,
	}
	queued, created, err := store.EnqueueRecoveredApprovalContinuation(ctx, approval.ID, input)
	if err != nil || !created || queued == nil {
		t.Fatalf("first recovery queued=%+v created=%v err=%v", queued, created, err)
	}
	if duplicate, created, err := store.EnqueueRecoveredApprovalContinuation(ctx, approval.ID, input); err != nil || created || duplicate != nil {
		t.Fatalf("second recovery queued=%+v created=%v err=%v", duplicate, created, err)
	}
	if rows, err := store.ListRecoverableApprovalDecisions(ctx, 10); err != nil || len(rows) != 0 {
		t.Fatalf("recoverable after repair=%+v err=%v", rows, err)
	}
	if _, _, _, claimed, err := store.ClaimApprovalResumeAuthorization(ctx, identity.TenantID, identity.PersonID, run.ID, "resume:v1:crash-window"); err != nil || !claimed {
		t.Fatalf("recovered authorization claimed=%v err=%v", claimed, err)
	}
}

func TestClaimedDecisionDoesNotCreateApprovalSpecificRecovery(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	approval, err := store.CreateApprovalRequest(ctx, ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		ActionType: "tool_call", AuthorizationFingerprint: "resume:v1:already-claimed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RespondApprovalRequest(ctx, identity.TenantID, identity.PersonID, approval.ID, "approved", "cli", ApprovalDecisionInput{DecisionID: "once"}); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := store.ClaimApprovalDecision(ctx, identity.TenantID, approval.ID, run.ID); err != nil || !claimed {
		t.Fatalf("claim decision: claimed=%v err=%v", claimed, err)
	}
	if _, err := store.MarkInterruptedRuns(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if rows, err := store.ListRecoverableApprovalDecisions(ctx, 10); err != nil || len(rows) != 0 {
		t.Fatalf("claimed approval generated duplicate recovery: rows=%+v err=%v", rows, err)
	}
}

func TestHistoricalDecisionWithoutRecoveryMarkerIsIgnored(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	approval, err := store.CreateApprovalRequest(ctx, ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		ActionType: "tool_call", AuthorizationFingerprint: "resume:v1:legacy-row",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RespondApprovalRequest(ctx, identity.TenantID, identity.PersonID, approval.ID, "approved", "cli", ApprovalDecisionInput{DecisionID: "once"}); err != nil {
		t.Fatal(err)
	}
	// Simulate a terminal approval created before crash-safe continuation was
	// introduced. The schema migration adds waiter_state=live, but does not and
	// must not arm old decisions for replay.
	if _, err := store.db.ExecContext(ctx, `UPDATE approval_requests SET decision_recorded_at = NULL WHERE id = ?`, approval.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkInterruptedRuns(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if rows, err := store.ListRecoverableApprovalDecisions(ctx, 10); err != nil || len(rows) != 0 {
		t.Fatalf("legacy decision generated recovery: rows=%+v err=%v", rows, err)
	}
	queued, created, err := store.EnqueueRecoveredApprovalContinuation(ctx, approval.ID, QueuedTask{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Platform: "cli", Channel: "cli",
		Content: "must not recover legacy decision", TaskID: task.ID,
		IdempotencyKey: "approval-resume:" + approval.ID,
	})
	if err != nil || created || queued != nil {
		t.Fatalf("legacy decision queued=%+v created=%v err=%v", queued, created, err)
	}
}
