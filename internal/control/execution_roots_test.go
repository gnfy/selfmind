package control

import (
	"context"
	"reflect"
	"testing"

	"selfmind/internal/executionenv"
)

func TestExecutionRootsRoundTripThroughQueueAndRun(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	roots := []executionenv.RootBinding{
		{Path: "/workspace", Role: executionenv.RootRolePrimary, AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceWorkspace, ContextRoot: true},
		{Path: "/shared", Role: executionenv.RootRoleAdditional, AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceCLIAddDir, ContextRoot: true},
	}
	queued, err := store.EnqueueQueued(ctx, QueuedTask{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		Platform: "cli", Channel: "cli", Content: "inspect", ExecutionRoots: roots,
	})
	if err != nil {
		t.Fatal(err)
	}
	loadedQueue, err := store.GetQueued(ctx, identity.TenantID, queued.ID)
	if err != nil || loadedQueue == nil {
		t.Fatalf("GetQueued = %#v, %v", loadedQueue, err)
	}
	if !reflect.DeepEqual(loadedQueue.ExecutionRoots, roots) {
		t.Fatalf("queue roots = %#v, want %#v", loadedQueue.ExecutionRoots, roots)
	}

	task, err := store.CreateTask(ctx, TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "root snapshot", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRunWithOptions(ctx, task, "cli", "inspect", StartRunOptions{ExecutionRoots: roots})
	if err != nil {
		t.Fatal(err)
	}
	loadedRun, err := store.GetRun(ctx, identity.TenantID, run.ID)
	if err != nil || loadedRun == nil {
		t.Fatalf("GetRun = %#v, %v", loadedRun, err)
	}
	if !reflect.DeepEqual(loadedRun.ExecutionRoots, roots) {
		t.Fatalf("run roots = %#v, want %#v", loadedRun.ExecutionRoots, roots)
	}
}
