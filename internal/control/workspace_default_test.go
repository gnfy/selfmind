package control

import (
	"context"
	"testing"
)

// TestEnsureWorkspaceDoesNotStealTheDefault pins the boundary between the two
// workspace notions: a session's directory and the person's durable default.
// Every local CLI turn ensures its own cwd, so an implicit default write here
// meant `ws default` could not survive the next command in another directory —
// and IM/cron work silently followed whichever terminal ran last.
func TestEnsureWorkspaceDoesNotStealTheDefault(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	// First ensure bootstraps the default: a fresh install must leave IM
	// somewhere to run.
	first, err2 := store.EnsureWorkspace(ctx, Workspace{
		TenantID: "t1", OwnerPersonID: "p1", LocalPath: "/w/alpha", Name: "alpha",
	})
	if err2 != nil {
		t.Fatal(err2)
	}
	current, err := store.CurrentWorkspace(ctx, "t1", "p1")
	if err != nil || current == nil {
		t.Fatalf("first ensure should bootstrap the default: %v %v", current, err)
	}
	if current.ID != first.ID {
		t.Fatalf("default = %s, want the only workspace %s", current.ID, first.ID)
	}

	// The person deliberately points IM and scheduled work at beta.
	beta, err := store.EnsureWorkspace(ctx, Workspace{
		TenantID: "t1", OwnerPersonID: "p1", LocalPath: "/w/beta", Name: "beta",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetCurrentWorkspace(ctx, "t1", "p1", beta.ID); err != nil {
		t.Fatal(err)
	}

	// Now a CLI turn runs in alpha. Ensuring alpha must NOT move the default.
	if _, err := store.EnsureWorkspace(ctx, Workspace{
		TenantID: "t1", OwnerPersonID: "p1", LocalPath: "/w/alpha", Name: "alpha",
	}); err != nil {
		t.Fatal(err)
	}
	current, err = store.CurrentWorkspace(ctx, "t1", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != beta.ID {
		t.Fatalf("ensuring alpha stole the default: got %s, want beta %s", current.ID, beta.ID)
	}

	// An EXPLICIT registration is a user act and may adopt.
	gamma, err := store.RegisterWorkspace(ctx, Workspace{
		TenantID: "t1", OwnerPersonID: "p1", LocalPath: "/w/gamma", Name: "gamma",
	})
	if err != nil {
		t.Fatal(err)
	}
	current, err = store.CurrentWorkspace(ctx, "t1", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != gamma.ID {
		t.Fatalf("explicit register should adopt: got %s, want gamma %s", current.ID, gamma.ID)
	}
}
