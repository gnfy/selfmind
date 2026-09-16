package control

import (
	"context"
	"testing"
	"time"
)

// A version-1 wait group could never settle once its run left running with
// fewer members than declared: the finished member stayed unfinalized, the
// person was never told, and the Run sat in waiting_external outside every
// Attention set (observed live for two days). Closing the check as blocked is
// the settlement modern groups already receive; it grants no continuation.
func TestLegacyWatchGroupSettlesBlockedOnceRunLeavesRunning(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	group, err := store.ResolveOrCreateExternalWatchGroup(ctx, identity.TenantID, identity.PersonID, task.ID, run.ID, "targets", ExternalWatchGroupAll, 2)
	if err != nil {
		t.Fatal(err)
	}
	watch, err := store.CreateExternalWatch(ctx, ExternalWatch{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		CWD: t.TempDir(), Command: "printf DONE", SuccessPattern: "DONE", WaitGroupID: group.ID,
		TimeoutAt: time.Now().Add(time.Hour), PreflightReceipt: ExternalWatchPreflightReceipt{Version: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinishExternalWatch(ctx, identity.TenantID, watch.ID, ExternalWatchSucceeded, "DONE", ""); err != nil {
		t.Fatal(err)
	}
	// While the run still runs, a legacy group keeps its original settlement:
	// the second member may still be registered.
	if r, err := store.ResolveExternalWatchGroup(ctx, identity.TenantID, group.ID, watch.ID); err != nil || r.Terminal {
		t.Fatalf("legacy group settled while its run was still registering: %+v %v", r, err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_external"); err != nil {
		t.Fatal(err)
	}
	r, err := store.ResolveExternalWatchGroup(ctx, identity.TenantID, group.ID, watch.ID)
	if err != nil || !r.Terminal || !r.Won || r.Status != ExternalWatchBlocked {
		t.Fatalf("legacy incomplete group must close as blocked once its run left running: %+v %v", r, err)
	}
	again, err := store.ResolveExternalWatchGroup(ctx, identity.TenantID, group.ID, watch.ID)
	if err != nil || again.Won || !again.Terminal || again.Status != ExternalWatchBlocked {
		t.Fatalf("duplicate resolution=%+v %v", again, err)
	}
}
