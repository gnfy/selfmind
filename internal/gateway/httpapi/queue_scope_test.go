package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/modelchange"
)

// TestQueuedWorkKeepsTheScopeItWasSubmittedWith pins where an agent is allowed
// to run. When the model is not ready and nothing is executing, the request is
// parked — and it used to be parked BEFORE the workspace and execution roots
// were resolved, so the queued row carried neither. Draining it later then fell
// back to the person's default workspace: work submitted from one repository
// could execute in another. The model-change drain path beside it already
// resolved the scope first; this one did not.
func TestQueuedWorkKeepsTheScopeItWasSubmittedWith(t *testing.T) {
	daemon, store, identity, _, _ := newApprovalTestServer(t)
	ctx := context.Background()

	// A model transaction service with no verified route: new work parks.
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("models:\n  primary:\n    provider: none\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	daemon.ModelChanges = &modelchange.Service{ConfigPath: configPath}
	if daemon.modelReadyForWork() {
		t.Fatal("fixture error: the model must be unready for this path to run")
	}

	submitted := t.TempDir()
	resp, _ := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: identity.PlatformUserID, Channel: "cli",
		Content: "build the thing", ClientCWD: submitted,
	})
	if !resp.Accepted {
		t.Fatalf("unready model should park the work, not reject it: %+v", resp)
	}

	queued, err := store.ListQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 {
		t.Fatalf("expected exactly one queued row, got %d", len(queued))
	}
	item := queued[0]
	if item.WorkspaceID == "" {
		t.Fatal("queued work carries no workspace; draining it would fall back to the person's default")
	}
	workspace, err := store.GetWorkspace(ctx, identity.TenantID, item.WorkspaceID)
	if err != nil || workspace == nil {
		t.Fatalf("queued workspace %q does not resolve: %v", item.WorkspaceID, err)
	}
	if workspace.LocalPath != submitted {
		t.Fatalf("queued workspace = %q, want the directory the work was submitted from (%q)",
			workspace.LocalPath, submitted)
	}
}
