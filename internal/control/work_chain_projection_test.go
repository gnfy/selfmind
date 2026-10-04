package control

import (
	"context"
	"testing"
	"time"
)

func TestWorkChainsUseDurableParentsIncludingManualClaims(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, root, other, _ := externalEffectRuns(t, store)
	if err := store.FinishRun(ctx, owner.TenantID, root.ID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	task, err := store.GetTask(ctx, owner.TenantID, root.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.StartRunWithOptions(ctx, task, "cli", "continue", StartRunOptions{ResumesRunID: root.ID, MaxActiveRuns: 3})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, owner.TenantID, child.ID, "done"); err != nil {
		t.Fatal(err)
	}
	// Remove only events: the committed edge remains the routing authority.
	if _, err := store.db.ExecContext(ctx, `DELETE FROM task_events WHERE run_id=? AND type='run.resumed'`, child.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE runs SET started_at=? ,finished_at=? WHERE id=?`, time.Now().Add(-48*time.Hour).Unix(), time.Now().Add(-47*time.Hour).Unix(), root.ID); err != nil {
		t.Fatal(err)
	}
	got, err := store.WorkChainsSince(ctx, owner.TenantID, owner.PersonID, time.Now().Add(-24*time.Hour), 100)
	if err != nil || got.ResumeEdges != 1 || got.LatestStatuses["done"] != 1 || got.Chains != got.Runs { // parent outside the window is identity only
		t.Fatalf("durable/window projection: %+v %v (other=%s)", got, err, other.ID)
	}
	got, err = store.WorkChainsSince(ctx, owner.TenantID, owner.PersonID, time.Now().Add(-72*time.Hour), 100)
	if err != nil || got.Chains != got.Runs-1 || got.ResumeEdges != 1 {
		t.Fatalf("manual edge was lost: %+v %v", got, err)
	}
	stranger, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "stranger-chain", "Stranger")
	if err != nil {
		t.Fatal(err)
	}
	got, err = store.WorkChainsSince(ctx, stranger.TenantID, stranger.PersonID, time.Now().Add(-72*time.Hour), 100)
	if err != nil || got.Runs != 0 || got.Chains != 0 {
		t.Fatalf("cross-person projection: %+v %v", got, err)
	}
}
