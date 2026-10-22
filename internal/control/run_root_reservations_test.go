package control

import (
	"context"
	"testing"
	"time"
)

func TestRunRootActivityExcludesExactResumeAncestorsOnly(t *testing.T) {
	ctx := context.Background()
	store, identity, task, ancestor := newRecoveryFixture(t)
	if err := store.FinishRun(ctx, identity.TenantID, ancestor.ID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	parent, err := store.StartRunWithOptions(ctx, task, "same-session", "first answer", StartRunOptions{ResumesRunID: ancestor.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, parent.ID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	current, err := store.StartRunWithOptions(ctx, task, "same-session", "second answer", StartRunOptions{ResumesRunID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListRunRootActivitySince(ctx, identity.TenantID, identity.PersonID, parent.ID, current.ID, time.Unix(0, 0))
	if err != nil || len(rows) != 0 {
		t.Fatalf("ancestor incorrectly counted as intervening work: %+v %v", rows, err)
	}
	other, err := store.StartRun(ctx, task, "other-session", "independent work under same grouping")
	if err != nil {
		t.Fatal(err)
	}
	rows, err = store.ListRunRootActivitySince(ctx, identity.TenantID, identity.PersonID, parent.ID, current.ID, time.Unix(0, 0))
	if err != nil || len(rows) != 1 || rows[0].RunID != other.ID {
		t.Fatalf("grouping hid unrelated work: %+v %v", rows, err)
	}
}

func TestRunRootReservationTracksExecutionNotParkedHumanWait(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	assertCount := func(want int) {
		t.Helper()
		rows, err := store.ListRunRootReservations(ctx, identity.TenantID, identity.PersonID)
		if err != nil || len(rows) != want {
			t.Fatalf("reservations=%+v want=%d err=%v", rows, want, err)
		}
	}
	assertCount(1)
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	assertCount(0) // Historical waiting_user alone grants no new reservation.
	approval, err := store.CreateApprovalRequest(ctx, ApprovalRequest{TenantID: identity.TenantID,
		PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID, ActionType: "tool"})
	if err != nil {
		t.Fatal(err)
	}
	cp := LoopCheckpointRecord{TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		Outcome: "await_human", Detail: "human_wait:approval:" + approval.ID, Snapshot: []byte(`[]`)}
	if err := store.SaveLoopCheckpoint(ctx, cp); err != nil {
		t.Fatal(err)
	}
	assertCount(0) // Diagnostic text is not a typed binding.
	cp.WaitKind, cp.WaitRef, cp.WaitToolCallID = LoopCheckpointWaitApproval, approval.ID, "call-human"
	if err := store.SaveLoopCheckpoint(ctx, cp); err != nil {
		t.Fatal(err)
	}
	assertCount(0)
	if rows, err := store.ListRunRootReservations(ctx, "another-tenant", identity.PersonID); err != nil || len(rows) != 0 {
		t.Fatalf("cross-tenant reservations=%+v %v", rows, err)
	}
	if rows, err := store.ListRunRootReservations(ctx, identity.TenantID, "another-person"); err != nil || len(rows) != 0 {
		t.Fatalf("cross-person reservations=%+v %v", rows, err)
	}
	cp.Outcome = "complete_turn"
	if err := store.SaveLoopCheckpoint(ctx, cp); err != nil {
		t.Fatal(err)
	}
	assertCount(0)
	cp.Outcome = "await_human"
	if err := store.SaveLoopCheckpoint(ctx, cp); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ExpireApprovalRequest(ctx, identity.TenantID, approval.ID, "question expired"); err != nil {
		t.Fatal(err)
	}
	assertCount(0)
}
