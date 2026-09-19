package control

import (
	"context"
	"testing"
	"time"
)

// A workspace class must be readable (it is the standing answer), must not
// leak into another workspace, and must be bounded by the cutoff a scheduled
// run carries. Without the cutoff a job created in January would inherit every
// class its owner accepted since.
func TestWorkspaceGrantScopeAndCutoff(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	const tenant, person, ws, other = "default", "p1", "ws-1", "ws-2"
	key := "exec:host|resource=workspace:abc:command:aws codebuild batch-get-builds"

	if err := store.GrantApproval(ctx, "workspace", tenant, person, ws, key, time.Time{}); err != nil {
		t.Fatal(err)
	}

	granted, err := store.IsApprovalGranted(ctx, tenant, person, ws, key, time.Time{})
	if err != nil || !granted {
		t.Fatalf("a workspace class must release its own workspace: %v %v", granted, err)
	}
	if granted, err = store.IsApprovalGranted(ctx, tenant, person, other, key, time.Time{}); err != nil || granted {
		t.Fatalf("a workspace class must not leak to another workspace: %v %v", granted, err)
	}
	if granted, err = store.IsApprovalGranted(ctx, tenant, person, "", key, time.Time{}); err != nil || granted {
		t.Fatalf("a caller with no workspace must not match a workspace class: %v %v", granted, err)
	}

	// Frozen before the grant existed: a schedule authorised earlier must not
	// pick it up.
	before := time.Now().Add(-time.Hour)
	if granted, err = store.IsApprovalGranted(ctx, tenant, person, ws, key, before); err != nil || granted {
		t.Fatalf("a class granted after the cutoff must not apply: %v %v", granted, err)
	}
	// Frozen after it: a schedule authorised later carries it.
	after := time.Now().Add(time.Hour)
	if granted, err = store.IsApprovalGranted(ctx, tenant, person, ws, key, after); err != nil || !granted {
		t.Fatalf("a class granted before the cutoff must apply: %v %v", granted, err)
	}
}
