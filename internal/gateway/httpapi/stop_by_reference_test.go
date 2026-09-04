package httpapi

import (
	"context"
	"strings"
	"testing"
)

// TestStopByOrdinalClearsWithoutRunning is the exit that attention items did not
// have. Dismissing one meant pinning it first — `/resume <n>` then `/stop` —
// which starts the work you were trying to put down, and automatic retention
// never touches anything with pending human input. Stale `waiting_user` residue
// therefore accumulated with no way to clear it (nine items, six a day old).
func TestStopByOrdinalClearsWithoutRunning(t *testing.T) {
	daemon, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()

	run, err := store.StartRun(ctx, task, "cli", "backfill the records")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}

	// An ordinal only means something against a listing the person has seen.
	if reply := daemon.dismissAttentionByReference(ctx, identity, "cli", "1"); !strings.Contains(reply, "run /resume first") {
		t.Fatalf("an ordinal with no listing must say so: %q", reply)
	}

	// An exact run id needs no listing.
	reply := daemon.dismissAttentionByReference(ctx, identity, "cli", run.ID)
	if strings.Contains(reply, "Could not") || strings.Contains(reply, "not yours") {
		t.Fatalf("dismissing an idle run by id should succeed: %q", reply)
	}

	// The run itself keeps its history: dismissal hides, it never rewrites.
	after, err := store.GetRun(ctx, identity.TenantID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "waiting_user" {
		t.Fatalf("dismissal must not rewrite run status, got %q", after.Status)
	}

	// Dismissing twice is not an error, but it does not claim a second success.
	if second := daemon.dismissAttentionByReference(ctx, identity, "cli", run.ID); !strings.Contains(second, "not current attention") {
		t.Fatalf("a second dismissal should report there was nothing to clear: %q", second)
	}
}

// A run belonging to someone else is never dismissible, and a malformed
// reference gets the usage line rather than a silent no-op.
func TestStopByReferenceRefusesWhatIsNotYours(t *testing.T) {
	daemon, _, identity, _, _ := newApprovalTestServer(t)
	ctx := context.Background()

	if reply := daemon.dismissAttentionByReference(ctx, identity, "cli", "run_someone_else"); !strings.Contains(reply, "not yours or no longer exists") {
		t.Fatalf("an unknown run must be refused: %q", reply)
	}
	if reply := daemon.dismissAttentionByReference(ctx, identity, "cli", "garbage"); !strings.Contains(reply, "Usage: /stop") {
		t.Fatalf("a malformed reference should show usage: %q", reply)
	}
}
