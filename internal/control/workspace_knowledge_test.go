package control

import (
	"context"
	"testing"
)

func TestReplaceWorkspaceKnowledgeRemovesStaleSections(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "tenant", "cli", "person", "User")
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := store.RegisterWorkspace(ctx, Workspace{
		TenantID: identity.TenantID, OwnerPersonID: identity.PersonID, Name: "repo", LocalPath: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	first := []WorkspaceKnowledgeWrite{
		{FilePath: "/repo/AGENTS.md", FileName: "AGENTS.md", ContentHash: "old", Section: 0, Title: "Build", Excerpt: "use make test"},
		{FilePath: "/repo/AGENTS.md", FileName: "AGENTS.md", ContentHash: "old", Section: 1, Title: "Deploy", Excerpt: "never deploy directly"},
	}
	if err := store.ReplaceWorkspaceKnowledge(ctx, identity.TenantID, identity.PersonID, workspace.ID, first); err != nil {
		t.Fatal(err)
	}
	second := []WorkspaceKnowledgeWrite{
		{FilePath: "/repo/AGENTS.md", FileName: "AGENTS.md", ContentHash: "new", Section: 0, Title: "Build", Excerpt: "use go test"},
	}
	if err := store.ReplaceWorkspaceKnowledge(ctx, identity.TenantID, identity.PersonID, workspace.ID, second); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListWorkspaceKnowledge(ctx, identity.TenantID, identity.PersonID, workspace.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ContentHash != "new" || rows[0].Excerpt != "use go test" {
		t.Fatalf("stale projection survived replacement: %+v", rows)
	}
}

func TestReplaceWorkspaceKnowledgeSkipsUnchangedProjection(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "tenant", "cli", "knowledge-noop", "User")
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := store.RegisterWorkspace(ctx, Workspace{
		TenantID: identity.TenantID, OwnerPersonID: identity.PersonID, Name: "repo", LocalPath: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	projection := []WorkspaceKnowledgeWrite{{
		FilePath: "/repo/AGENTS.md", FileName: "AGENTS.md", ContentHash: "stable",
		Section: 0, Title: "Build", Excerpt: "use go test",
	}}
	if err := store.ReplaceWorkspaceKnowledge(ctx, identity.TenantID, identity.PersonID, workspace.ID, projection); err != nil {
		t.Fatal(err)
	}
	var before, after int64
	if err := store.db.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceWorkspaceKnowledge(ctx, identity.TenantID, identity.PersonID, workspace.ID, projection); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("unchanged projection must not rewrite rows: before=%d after=%d", before, after)
	}
}

func TestReplaceWorkspaceKnowledgeRejectsForeignWorkspace(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	owner, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "owner", "Owner")
	other, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "other", "Other")
	workspace, err := store.RegisterWorkspace(ctx, Workspace{
		TenantID: owner.TenantID, OwnerPersonID: owner.PersonID, Name: "private", LocalPath: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = store.ReplaceWorkspaceKnowledge(ctx, other.TenantID, other.PersonID, workspace.ID, []WorkspaceKnowledgeWrite{{
		FilePath: "/repo/AGENTS.md", FileName: "AGENTS.md", ContentHash: "hash", Section: 0, Excerpt: "private rule",
	}})
	if err == nil {
		t.Fatal("foreign workspace knowledge write must be rejected")
	}
}
