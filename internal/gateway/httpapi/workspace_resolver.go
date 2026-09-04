package httpapi

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"selfmind/internal/control"
)

// Workspace-reference resolution for the /workspace control command, mirroring
// the approval resolver contract (approval_resolver.go): the number shown by
// /workspaces resolves the same workspace on every surface, because listing
// and resolution share ONE display-ordered fetch (display order = resolution
// order). Full workspace ids keep working unchanged.

// listWorkspacesForDisplay is the single fetch both /workspaces rendering and
// /workspace <n> ordinal resolution use. The store orders by updated_at DESC;
// the name/id tiebreak here makes ties deterministic so the numbered list and
// ordinal resolution can never disagree within one snapshot.
func (d *Server) listWorkspacesForDisplay(ctx context.Context, identity *control.IdentityContext) ([]control.Workspace, error) {
	workspaces, err := d.Control.ListWorkspaces(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(workspaces, func(i, j int) bool {
		if !workspaces[i].UpdatedAt.Equal(workspaces[j].UpdatedAt) {
			return workspaces[i].UpdatedAt.After(workspaces[j].UpdatedAt)
		}
		return workspaces[i].ID < workspaces[j].ID
	})
	return workspaces, nil
}

// resolveWorkspaceReference resolves a user-supplied workspace reference: a
// bare number is a LIST ORDINAL against the /workspaces display order, and
// anything else is a workspace id. Only the person's own workspaces resolve.
// The middle return is a user-facing sentence for reference mistakes (safe to
// send verbatim on any channel); the error return is reserved for storage
// failures.
func (d *Server) resolveWorkspaceReference(ctx context.Context, identity *control.IdentityContext, ref string) (*control.Workspace, string, error) {
	ref = strings.TrimSpace(ref)
	if ordinal, convErr := strconv.Atoi(ref); convErr == nil {
		workspaces, err := d.listWorkspacesForDisplay(ctx, identity)
		if err != nil {
			return nil, "", err
		}
		if len(workspaces) == 0 {
			return nil, "No workspaces registered; see /ws.", nil
		}
		if ordinal < 1 || ordinal > len(workspaces) {
			return nil, fmt.Sprintf("No workspace number %d; %d listed (see /ws).", ordinal, len(workspaces)), nil
		}
		return &workspaces[ordinal-1], "", nil
	}
	ws, err := d.Control.GetWorkspace(ctx, identity.TenantID, ref)
	if err != nil {
		return nil, "", err
	}
	if ws == nil || ws.OwnerPersonID != identity.PersonID {
		return nil, "Workspace not found. Run /ws to list them.", nil
	}
	return ws, "", nil
}

// resumeWorkspaceNote renders the workspace-binding suffix of the /resume
// success reply. The resumed task's workspace only binds on the NEXT agent
// turn, so the client's status bar keeps showing the launch cwd; stating the
// binding here is what tells the user the resume actually worked.
func (d *Server) resumeWorkspaceNote(ctx context.Context, identity *control.IdentityContext, task *control.Task) string {
	if task == nil || task.WorkspaceID == "" {
		return " — no workspace bound; your next message uses the current workspace."
	}
	if ws, err := d.Control.GetWorkspace(ctx, identity.TenantID, task.WorkspaceID); err == nil && ws != nil {
		return fmt.Sprintf(" — workspace: %s (%s); your next message runs there.", ws.Name, ws.LocalPath)
	}
	// Best-effort: a missing workspace row still names the binding.
	return fmt.Sprintf(" — workspace: %s; your next message runs there.", task.WorkspaceID)
}
