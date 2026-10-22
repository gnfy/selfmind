package control

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"selfmind/internal/executionenv"
)

func TestExecutionViewUsageSeparatesPendingWorkFromRunHistory(t *testing.T) {
	for _, viewName := range []string{"view_0123-abcd", "view with space"} {
		t.Run(viewName, func(t *testing.T) {
			ctx := context.Background()
			store := newTestStore(t)
			identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "owner", "Owner")
			if err != nil {
				t.Fatal(err)
			}
			task, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "view", Channel: "cli"})
			if err != nil {
				t.Fatal(err)
			}
			views := t.TempDir()
			path := filepath.Join(views, viewName)
			bind := func(p string) []executionenv.RootBinding {
				return []executionenv.RootBinding{{Path: p, Role: executionenv.RootRolePrimary, AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceExecutionView}}
			}
			start := func(roots []executionenv.RootBinding, status string) string {
				t.Helper()
				run, err := store.StartRunWithOptions(ctx, task, "cli", "work", StartRunOptions{ExecutionRoots: roots})
				if err != nil {
					t.Fatal(err)
				}
				if status != "running" {
					if err := store.FinishRun(ctx, identity.TenantID, run.ID, status); err != nil {
						t.Fatal(err)
					}
				}
				return run.ID
			}
			done := start(bind(path), "done")
			start(bind(filepath.Join(views, viewName+"-other")), "waiting_user")
			usage, err := store.ExecutionViewUsage(ctx, identity.TenantID, identity.PersonID, path)
			if err != nil || usage.Pending || !slices.Equal(usage.RunIDs, []string{done}) {
				t.Fatalf("finished history = %+v %v", usage, err)
			}
			parked := start(bind(path+string(filepath.Separator)), "waiting_user")
			usage, err = store.ExecutionViewUsage(ctx, identity.TenantID, identity.PersonID, path)
			want := []string{done, parked}
			slices.Sort(want)
			if err != nil || !usage.Pending || !slices.Equal(usage.RunIDs, want) {
				t.Fatalf("parked Run was not pending: %+v %v", usage, err)
			}
			if err := store.FinishRun(ctx, identity.TenantID, parked, "cancelled"); err != nil {
				t.Fatal(err)
			}
			queued, err := store.EnqueueQueued(ctx, QueuedTask{TenantID: identity.TenantID, PersonID: identity.PersonID,
				Platform: "cli", Channel: "other", Content: "follow up", ExecutionRoots: bind(path)})
			if err != nil {
				t.Fatal(err)
			}
			if usage, err := store.ExecutionViewUsage(ctx, identity.TenantID, identity.PersonID, path); err != nil || !usage.Pending {
				t.Fatalf("queued follow-up was not pending: %+v %v", usage, err)
			}
			if err := store.MarkQueued(ctx, identity.TenantID, queued.ID, QueueStatusCancelled); err != nil {
				t.Fatal(err)
			}
			if usage, err := store.ExecutionViewUsage(ctx, identity.TenantID, identity.PersonID, path); err != nil || usage.Pending || len(usage.RunIDs) != 2 {
				t.Fatalf("settled references stayed pending: %+v %v", usage, err)
			}
			stranger, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "stranger", "Stranger")
			if err != nil {
				t.Fatal(err)
			}
			if usage, err := store.ExecutionViewUsage(ctx, stranger.TenantID, stranger.PersonID, path); err != nil || usage.Pending || len(usage.RunIDs) != 0 {
				t.Fatalf("another person's view usage leaked: %+v %v", usage, err)
			}
		})
	}
}
