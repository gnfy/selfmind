package control

import (
	"context"
	"testing"
	"time"
)

func TestExternalWatchLifecycle(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	watch, err := store.CreateExternalWatch(ctx, ExternalWatch{
		TenantID:              identity.TenantID,
		PersonID:              identity.PersonID,
		TaskID:                task.ID,
		RunID:                 run.ID,
		Channel:               "cli",
		Description:           "CI build",
		CWD:                   t.TempDir(),
		Command:               "check-build",
		SuccessPattern:        "SUCCEEDED",
		FailurePattern:        "FAILED",
		IntervalSeconds:       5,
		CommandTimeoutSeconds: 10,
		TimeoutAt:             time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	due, err := store.ListDueExternalWatches(ctx, 10)
	if err != nil || len(due) != 1 || due[0].ID != watch.ID {
		t.Fatalf("due = %+v err=%v", due, err)
	}
	claimed, err := store.ClaimExternalWatch(ctx, due[0])
	if err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	if claimed, err := store.ClaimExternalWatch(ctx, due[0]); err != nil || claimed {
		t.Fatalf("duplicate claim: claimed=%v err=%v", claimed, err)
	}
	if err := store.RecordExternalWatchCheck(ctx, watch.TenantID, watch.ID, "still running", ""); err != nil {
		t.Fatal(err)
	}
	finished, err := store.FinishExternalWatch(ctx, watch.TenantID, watch.ID, ExternalWatchSucceeded, "SUCCEEDED", "")
	if err != nil || !finished {
		t.Fatalf("finish: finished=%v err=%v", finished, err)
	}
	if finished, err := store.FinishExternalWatch(ctx, watch.TenantID, watch.ID, ExternalWatchFailed, "", "late failure"); err != nil || finished {
		t.Fatalf("terminal watch changed twice: finished=%v err=%v", finished, err)
	}
	counts, err := store.CountExternalWatchesByStatus(ctx, identity.TenantID, identity.PersonID)
	if err != nil || counts[ExternalWatchSucceeded] != 1 {
		t.Fatalf("counts = %+v err=%v", counts, err)
	}
}
