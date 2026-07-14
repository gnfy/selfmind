package httpapi

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
)

// /workspace <n> resolves the number the /workspaces listing printed —
// display order = resolution order, mirroring the approval ordinal contract.

func seedWorkspace(t *testing.T, store *control.Store, identity *control.IdentityContext, name string) *control.Workspace {
	t.Helper()
	ws, err := store.RegisterWorkspace(context.Background(), control.Workspace{
		TenantID: identity.TenantID, OwnerPersonID: identity.PersonID,
		Name: name, LocalPath: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

// TestWorkspaceOrdinalResolvesListedOrder: after /workspaces renders the
// numbered list, /workspace <n> selects exactly the workspace printed as n
// (observed live: /workspace 2 → "Workspace not found." ×3).
func TestWorkspaceOrdinalResolvesListedOrder(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	seedWorkspace(t, store, identity, "alpha")
	seedWorkspace(t, store, identity, "beta")

	listed, err := daemon.listWorkspacesForDisplay(context.Background(), identity)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("want 2 workspaces, got %d", len(listed))
	}

	// The rendered list and the resolver must agree on who number 2 is.
	out := controlReply(t, daemon, "/workspaces")
	if !strings.Contains(out, "2. "+listed[1].Name+" ("+listed[1].ID+")") {
		t.Fatalf("/workspaces numbering disagrees with display order:\n%s", out)
	}
	switched := controlReply(t, daemon, "/workspace 2")
	if !strings.Contains(switched, "Current workspace: "+listed[1].Name) || !strings.Contains(switched, listed[1].ID) {
		t.Fatalf("/workspace 2 selected the wrong workspace: %s", switched)
	}
	current, err := store.CurrentWorkspace(context.Background(), identity.TenantID, identity.PersonID)
	if err != nil || current == nil || current.ID != listed[1].ID {
		t.Fatalf("current workspace not switched: %+v err=%v", current, err)
	}

	// Full ids keep working.
	byID := controlReply(t, daemon, "/workspace "+listed[0].ID)
	if !strings.Contains(byID, "Current workspace: "+listed[0].Name) {
		t.Fatalf("/workspace <id> broke: %s", byID)
	}

	// Out-of-range ordinal is a reference mistake pointing back at the list.
	if out := controlReply(t, daemon, "/workspace 5"); !strings.Contains(out, "No workspace number 5; 2 listed") {
		t.Fatalf("out-of-range reply: %s", out)
	}
	if out := controlReply(t, daemon, "/workspace ws_nope"); !strings.Contains(out, "Workspace not found") {
		t.Fatalf("unknown id reply: %s", out)
	}
}

// TestWorkspaceUnifiedVerbAndAlias: /workspace, /workspaces, and /ws behave
// identically — bare lists, an argument selects. /ws is the short alias.
func TestWorkspaceUnifiedVerbAndAlias(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	seedWorkspace(t, store, identity, "alpha")
	seedWorkspace(t, store, identity, "beta")

	// Bare forms all list, and list the same set.
	list := controlReply(t, daemon, "/workspaces")
	for _, bare := range []string{"/workspace", "/ws"} {
		if got := controlReply(t, daemon, bare); got != list {
			t.Fatalf("%q must list identically to /workspaces:\n got: %s\nwant: %s", bare, got, list)
		}
	}

	// Resolve #2 against the actual display order (order = resolution order).
	listed, err := daemon.listWorkspacesForDisplay(context.Background(), identity)
	if err != nil || len(listed) != 2 {
		t.Fatalf("want 2 workspaces: %v err=%v", listed, err)
	}
	second := listed[1]

	// /ws <n> selects exactly like /workspace <n>, and sets current workspace.
	switched := controlReply(t, daemon, "/ws 2")
	if !strings.Contains(switched, "Current workspace: "+second.Name) || !strings.Contains(switched, second.ID) {
		t.Fatalf("/ws 2 selected the wrong workspace: %s", switched)
	}
	current, err := store.CurrentWorkspace(context.Background(), identity.TenantID, identity.PersonID)
	if err != nil || current == nil || current.ID != second.ID {
		t.Fatalf("/ws 2 did not switch current workspace: %+v err=%v", current, err)
	}

	// /ws <id> also selects.
	if byID := controlReply(t, daemon, "/ws "+second.ID); !strings.Contains(byID, "Current workspace: "+second.Name) {
		t.Fatalf("/ws <id> broke: %s", byID)
	}
}
