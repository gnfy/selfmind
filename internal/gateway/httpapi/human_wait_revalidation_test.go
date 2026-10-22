package httpapi

import (
	"context"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/kernel"
)

func TestHumanResumeRevalidationUsesPhysicalScopeAndCurrentOwnership(t *testing.T) {
	for _, kind := range []string{"overlap", "independent", "read-only", "unknown", "query-failure"} {
		t.Run(kind, func(t *testing.T) {
			daemon, store, _, owner := newHumanWaitServer(t)
			ctx := context.Background()
			ws := humanWaitWorkspace(t, store, owner, "original")
			task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, WorkspaceID: ws.ID, Title: "original"})
			if err != nil {
				t.Fatal(err)
			}
			roots := []executionenv.RootBinding{{Path: ws.LocalPath, AccessCap: executionenv.RootAccessWrite}}
			parent, err := store.StartRunWithOptions(ctx, task, "old", "request", control.StartRunOptions{ExecutionRoots: roots})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.FinishRun(ctx, owner.TenantID, parent.ID, "waiting_user"); err != nil {
				t.Fatal(err)
			}
			otherRoots := executionenv.CloneRootBindings(roots)
			switch kind {
			case "independent":
				otherRoots[0].Path = t.TempDir()
			case "read-only":
				otherRoots[0].AccessCap = executionenv.RootAccessRead
			case "unknown":
				otherRoots = nil
			}
			current, err := store.StartRunWithOptions(ctx, task, "origin", "resume", control.StartRunOptions{ExecutionRoots: roots})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.StartRunWithOptions(ctx, task, "other", "work", control.StartRunOptions{ExecutionRoots: otherRoots}); err != nil {
				t.Fatal(err)
			}
			ctx = kernel.WithToolInvocationScope(ctx, kernel.ToolInvocationScope{RunID: current.ID})
			if kind == "query-failure" {
				store.Close()
			}
			reason := daemon.coordinator().humanResumeRecheckReason(ctx, owner, parent, &control.LoopCheckpointRecord{UpdatedAt: time.Now().Add(-time.Second)})
			want := kind != "independent" && kind != "read-only"
			if (reason != "") != want {
				t.Fatalf("kind=%s reason=%q", kind, reason)
			}
		})
	}
}
