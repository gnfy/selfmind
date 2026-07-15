package httpapi

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
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

func TestWorkspaceIMRepliesAreReadableAndHideIDs(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx_user", "Weixin User")
	if err != nil {
		t.Fatal(err)
	}
	first := seedWorkspace(t, store, identity, "alpha")
	second := seedWorkspace(t, store, identity, "beta")
	if err := store.SetCurrentWorkspace(ctx, identity.TenantID, identity.PersonID, second.ID); err != nil {
		t.Fatal(err)
	}
	daemon := &Server{Control: store, DefaultTenantID: "default"}
	listed, err := daemon.listWorkspacesForDisplay(ctx, identity)
	if err != nil || len(listed) != 2 {
		t.Fatalf("list workspaces: %+v err=%v", listed, err)
	}

	listResp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "weixin", PlatformUserID: "wx_user", Channel: "wx_chat", Content: "/ws",
	})
	if status != 200 || listResp.Error != "" {
		t.Fatalf("/ws status=%d resp=%+v", status, listResp)
	}
	if strings.Contains(listResp.Content, first.ID) || strings.Contains(listResp.Content, second.ID) {
		t.Fatalf("IM workspace list must hide UUIDs:\n%s", listResp.Content)
	}
	if !strings.Contains(listResp.Content, "WS\n\n[1]") ||
		!strings.Contains(listResp.Content, "\n[2]") ||
		!strings.Contains(listResp.Content, "beta [current] | "+second.LocalPath) {
		t.Fatalf("IM workspace list is not grouped/readable:\n%s", listResp.Content)
	}
	for _, unstable := range []string{"\n1. ", "\n2. ", "\n   ", "Path:", "<number>"} {
		if strings.Contains(listResp.Content, unstable) {
			t.Fatalf("IM workspace list contains rich-text-sensitive syntax %q:\n%s", unstable, listResp.Content)
		}
	}

	switchResp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "weixin", PlatformUserID: "wx_user", Channel: "wx_chat", Content: "/ws 1",
	})
	if status != 200 || switchResp.Error != "" {
		t.Fatalf("/ws 1 status=%d resp=%+v", status, switchResp)
	}
	target := listed[0]
	if strings.Contains(switchResp.Content, target.ID) ||
		!strings.Contains(switchResp.Content, "WS switched\n\n"+target.Name+"\n"+target.LocalPath) {
		t.Fatalf("IM switch reply is not compact/readable:\n%s", switchResp.Content)
	}
}
