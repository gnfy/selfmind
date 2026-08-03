package app

import (
	"context"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/tools"
)

// The boot sweep must withdraw exactly the over-broad shapes and leave real
// command families alone. The input mirrors the ten person-scope host grants
// recorded on 2026-07-28 plus one older resource-less row.
func TestReviewApprovalGrantsWithdrawsOverBroadClasses(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	const person = "person_1"
	hostKey := func(family string) string {
		return "exec:" + tools.HostEscapeApprovalReason +
			"|resource=workspace:7a417b1b00a598fc:command:" + family
	}
	withdraw := []string{hostKey("set"), hostKey("for"), hostKey("python3"), hostKey("git"), hostKey("find")}
	keepKeys := []string{hostKey("gcloud"), hostKey("kubectl"), hostKey("aws"), hostKey("gh"), hostKey("argocd")}
	legacy := "exec:" + tools.HostEscapeApprovalReason

	for _, key := range append(append([]string{legacy}, withdraw...), keepKeys...) {
		if err := store.GrantApproval(ctx, "person", control.DefaultTenantID, person, person, key, time.Time{}); err != nil {
			t.Fatalf("seed grant %q: %v", key, err)
		}
	}

	revoked, kept, err := ReviewApprovalGrants(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if want := len(withdraw) + 1; revoked != want {
		t.Fatalf("revoked = %d, want %d", revoked, want)
	}
	if len(kept) != len(keepKeys) {
		t.Fatalf("kept = %d grants, want %d", len(kept), len(keepKeys))
	}
	for _, key := range withdraw {
		granted, err := store.IsApprovalGranted(ctx, control.DefaultTenantID, person, "", key)
		if err != nil {
			t.Fatal(err)
		}
		if granted {
			t.Fatalf("key %q must no longer authorize its class", key)
		}
	}
	for _, key := range keepKeys {
		granted, err := store.IsApprovalGranted(ctx, control.DefaultTenantID, person, "", key)
		if err != nil {
			t.Fatal(err)
		}
		if !granted {
			t.Fatalf("key %q is a legitimate command family and must survive", key)
		}
	}

	// Idempotent: a second boot withdraws nothing further.
	revokedAgain, _, err := ReviewApprovalGrants(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if revokedAgain != 0 {
		t.Fatalf("second sweep revoked %d grants, want 0", revokedAgain)
	}
}

func TestReviewApprovalGrantsHandlesNilStore(t *testing.T) {
	if revoked, kept, err := ReviewApprovalGrants(context.Background(), nil); err != nil || revoked != 0 || kept != nil {
		t.Fatalf("nil store must be a no-op, got (%d, %v, %v)", revoked, kept, err)
	}
}
