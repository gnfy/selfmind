package httpapi

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
)

// WorkspaceTrustDeclined marks a workspace the person was asked about and chose
// not to trust. It reuses trust_source rather than adding a third trust level:
// the level is still untrusted, and this only records WHY, which is what stops
// the startup prompt from asking again.
const WorkspaceTrustDeclined = "user_declined"

// ensureSessionWorkspace resolves the workspace a local session is running in,
// creating it when the directory has none yet.
//
// Control commands are handled BEFORE the run pipeline calls
// prepareRequestWorkspace, so without this a `/ws` from a fresh directory did
// not even list the directory it was standing in.
//
// Only a local CLI request carries a trustworthy directory; IM and scheduled
// turns fall back to the person's durable default.
func (d *Server) ensureSessionWorkspace(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest) *control.Workspace {
	if d == nil || d.Control == nil || identity == nil {
		return nil
	}
	if id := strings.TrimSpace(req.WorkspaceID); id != "" {
		ws, _ := d.Control.GetWorkspace(ctx, identity.TenantID, id)
		return ws
	}
	if !isLocalCLIRequest(req) {
		ws, _ := d.Control.CurrentWorkspace(ctx, identity.TenantID, identity.PersonID)
		return ws
	}
	cwd := cleanClientCWD(req.ClientCWD)
	if cwd == "" {
		ws, _ := d.Control.CurrentWorkspace(ctx, identity.TenantID, identity.PersonID)
		return ws
	}
	ws, err := d.Control.EnsureWorkspace(ctx, control.Workspace{
		TenantID:      identity.TenantID,
		OwnerPersonID: identity.PersonID,
		Name:          filepath.Base(cwd),
		LocalPath:     cwd,
		AllowedRoots:  []string{cwd},
	})
	if err != nil {
		return nil
	}
	return ws
}

// sessionWorkspaceTrustNote states what an untrusted workspace costs and how to
// change it, or "" when there is nothing to say.
//
// Untrusted is not a warning about danger; it is a capability fact. Saying only
// "[untrusted]" left two capabilities silently switched off with no hint that
// they existed or how to get them.
func sessionWorkspaceTrustNote(ws *control.Workspace) string {
	if ws == nil || ws.TrustLevel != executionenv.TrustUntrusted {
		return ""
	}
	return "This workspace is untrusted, so workspace Skills and remembered approval observations stay off. `/ws trust` enables them; `/ws decline` stops asking."
}

// digestWorkspaceFrom is the one typed form of a workspace that clients read
// trust from. Both the attach digest and workspace-selecting control commands
// publish it, so a client never has to parse a reply sentence to learn whether
// the trust question is still open.
func digestWorkspaceFrom(ws *control.Workspace) *api.DigestWorkspace {
	if ws == nil {
		return nil
	}
	trusted := ws.TrustLevel == executionenv.TrustTrusted
	return &api.DigestWorkspace{
		ID: ws.ID, Name: ws.Name, Path: ws.LocalPath, Trusted: trusted,
		// Trusting is itself an answer; declining is recorded as the source so
		// a "no" is not mistaken for "not asked yet".
		TrustAsked: trusted || ws.TrustSource == WorkspaceTrustDeclined,
	}
}

// workspaceTrustReply applies a trust decision to the session's workspace.
func (d *Server) workspaceTrustReply(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, action string) (string, *api.DigestWorkspace, error) {
	ws := d.ensureSessionWorkspace(ctx, identity, req)
	if ws == nil {
		return "No workspace for this session yet.", nil, nil
	}
	level, source := executionenv.TrustTrusted, "local_cli"
	switch action {
	case "trust":
	case "untrust":
		level, source = executionenv.TrustUntrusted, "local_cli"
	case "decline":
		level, source = executionenv.TrustUntrusted, WorkspaceTrustDeclined
	default:
		return "Usage: /ws trust | /ws untrust | /ws decline", nil, nil
	}
	updated, err := d.Control.SetWorkspaceTrust(ctx, identity.TenantID, identity.PersonID, ws.ID, level, source)
	if err != nil {
		return "", nil, err
	}
	if updated == nil {
		return "Workspace not found.", nil, nil
	}
	published := digestWorkspaceFrom(updated)
	switch action {
	case "trust":
		return fmt.Sprintf("Trusted %s (%s)\n%s", updated.Name, updated.ID, updated.LocalPath), published, nil
	case "untrust":
		return fmt.Sprintf("No longer trusted: %s (%s)", updated.Name, updated.ID), published, nil
	default:
		return fmt.Sprintf("Left untrusted and will not ask again: %s (%s)", updated.Name, updated.ID), published, nil
	}
}
