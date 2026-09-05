package httpapi

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
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
	out := controlReply(t, daemon, "/ws")
	if !strings.Contains(out, "2. "+listed[1].Name+" ("+listed[1].ID+")") {
		t.Fatalf("/ws numbering disagrees with display order:\n%s", out)
	}
	switched := controlReply(t, daemon, "/ws 2")
	if !strings.Contains(switched, "Current workspace: "+listed[1].Name) || !strings.Contains(switched, listed[1].ID) {
		t.Fatalf("/ws 2 selected the wrong workspace: %s", switched)
	}
	// Selecting for the session must NOT move the person's durable default:
	// two terminals in different projects were overwriting one person-level
	// value, so each one's list showed the other's choice as current while its
	// own work ran elsewhere.
	// Anchor the default somewhere else first, so "unchanged" is provable.
	if err := store.SetCurrentWorkspace(context.Background(), identity.TenantID, identity.PersonID, listed[0].ID); err != nil {
		t.Fatal(err)
	}
	if _ = controlReply(t, daemon, "/ws 2"); true {
		after, err := store.CurrentWorkspace(context.Background(), identity.TenantID, identity.PersonID)
		if err != nil || after == nil || after.ID != listed[0].ID {
			t.Fatalf("a session switch moved the durable default: %+v err=%v", after, err)
		}
	}

	// The durable default has its own verb.
	if out := controlReply(t, daemon, "/ws default 2"); !strings.Contains(out, "Default workspace") {
		t.Fatalf("/ws default reply: %s", out)
	}
	after, err := store.CurrentWorkspace(context.Background(), identity.TenantID, identity.PersonID)
	if err != nil || after == nil || after.ID != listed[1].ID {
		t.Fatalf("default workspace not switched: %+v err=%v", after, err)
	}

	// Full ids keep working.
	byID := controlReply(t, daemon, "/ws "+listed[0].ID)
	if !strings.Contains(byID, "Current workspace: "+listed[0].Name) {
		t.Fatalf("/ws <id> broke: %s", byID)
	}

	// Out-of-range ordinal is a reference mistake pointing back at the list.
	if out := controlReply(t, daemon, "/ws 5"); !strings.Contains(out, "No workspace number 5; 2 listed") {
		t.Fatalf("out-of-range reply: %s", out)
	}
	if out := controlReply(t, daemon, "/ws ws_nope"); !strings.Contains(out, "Workspace not found") {
		t.Fatalf("unknown id reply: %s", out)
	}
}

// TestWorkspaceHasExactlyOneVerb: /ws is the only workspace command. It used to
// have three interchangeable spellings (/workspace, /workspaces, /ws); three
// names for one thing is something to read, remember, and keep in sync, and the
// retired spellings must answer as unknown rather than silently working.
func TestWorkspaceHasExactlyOneVerb(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	seedWorkspace(t, store, identity, "alpha")
	seedWorkspace(t, store, identity, "beta")

	list := controlReply(t, daemon, "/ws")
	if !strings.Contains(list, "alpha") || !strings.Contains(list, "beta") {
		t.Fatalf("bare /ws must list workspaces:\n%s", list)
	}
	for _, retired := range []string{"/workspace", "/workspaces", "/workspace 2"} {
		if got := controlReply(t, daemon, retired); !strings.Contains(got, "Unknown command") {
			t.Fatalf("%q must be retired, got: %s", retired, got)
		}
	}

	listed, err := daemon.listWorkspacesForDisplay(context.Background(), identity)
	if err != nil || len(listed) != 2 {
		t.Fatalf("want 2 workspaces: %v err=%v", listed, err)
	}
	switched := controlReply(t, daemon, "/ws 2")
	if !strings.Contains(switched, "Current workspace: "+listed[1].Name) {
		t.Fatalf("/ws 2 selected the wrong workspace: %s", switched)
	}
	// The listing distinguishes the session from the durable default, so two
	// terminals in different projects cannot present each other's choice as
	// their own.
	if out := controlReply(t, daemon, "/ws"); !strings.Contains(out, "← ") {
		t.Fatalf("the listing must mark where this session is:\n%s", out)
	}
}

func TestWorkspaceIMRepliesAreReadableAndHideIDs(t *testing.T) {
	store := controltest.NewStore(t)
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
