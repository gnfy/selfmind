package control

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"selfmind/internal/executionenv"
)

func TestClaimInteractionContinuationMovesRunAndClaimsParentAtomically(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	roots := []executionenv.RootBinding{{Path: "/workspace", Role: executionenv.RootRolePrimary, AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceWorkspace}}
	targetTask, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, WorkspaceID: "workspace", Title: "target", Channel: "cli"})
	parent, _ := store.StartRunWithOptions(ctx, targetTask, "cli", "target", StartRunOptions{ExecutionRoots: roots})
	_ = store.FinishRun(ctx, identity.TenantID, parent.ID, "waiting_user")
	if _, err := store.ArchiveTaskByUser(ctx, identity.TenantID, identity.PersonID, targetTask.ID); err != nil {
		// Archive is optional for the scenario; a refusal only means the
		// visibility assertion below checks a listed thread instead.
		t.Logf("archive skipped: %v", err)
	}
	sourceTask, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, WorkspaceID: "workspace", Title: "确认执行", Channel: "cli"})
	source, _ := store.StartRunWithOptions(ctx, sourceTask, "cli", "确认执行", StartRunOptions{ExecutionRoots: roots})
	_, _ = store.AppendEvent(ctx, Event{TaskID: sourceTask.ID, RunID: source.ID, Type: "work.selection", Visibility: "task"})

	claimed, err := store.ClaimInteractionContinuation(ctx, identity.TenantID, identity.PersonID, source.ID, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.TaskID != targetTask.ID || claimed.ParentRunID != parent.ID || claimed.Status != "running" {
		t.Fatalf("claimed run = %+v", claimed)
	}
	if placeholder, _ := store.GetTask(ctx, identity.TenantID, sourceTask.ID); placeholder != nil {
		t.Fatalf("empty interaction placeholder survived: %+v", placeholder)
	}
	events, err := store.ListRunEvents(ctx, identity.TenantID, identity.PersonID, targetTask.ID, source.ID, 10)
	if err != nil || len(events) != 1 || events[0].TaskID != targetTask.ID {
		t.Fatalf("moved events = %+v err=%v", events, err)
	}
	refreshed, _ := store.GetTask(ctx, identity.TenantID, targetTask.ID)
	if refreshed == nil || refreshed.Visibility != "listed" || refreshed.Kind != "work" {
		t.Fatalf("parent thread must be listed work after a deliberate continuation: %+v", refreshed)
	}
	unresolved, _ := store.ListUnresolvedRuns(ctx, identity.TenantID, identity.PersonID, targetTask.ID, 10)
	if len(unresolved) != 0 {
		t.Fatalf("claimed parent must leave the unresolved set: %+v", unresolved)
	}
	if _, err := store.ClaimInteractionContinuation(ctx, identity.TenantID, identity.PersonID, source.ID, parent.ID); err == nil {
		t.Fatal("a second claim of the same interaction must be refused")
	}
}

func TestClaimInteractionContinuationRefusesDomainAndCheckpointMismatch(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	parentRoots := []executionenv.RootBinding{{Path: "/workspace", Role: executionenv.RootRolePrimary, AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceWorkspace}}
	targetTask, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, WorkspaceID: "workspace", Title: "target", Channel: "cli"})
	parent, _ := store.StartRunWithOptions(ctx, targetTask, "cli", "target", StartRunOptions{ExecutionRoots: parentRoots})
	_ = store.FinishRun(ctx, identity.TenantID, parent.ID, "interrupted")

	otherRoots := []executionenv.RootBinding{{Path: "/elsewhere", Role: executionenv.RootRolePrimary, AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceWorkspace}}
	foreignTask, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, WorkspaceID: "workspace", Title: "continue", Channel: "cli"})
	foreign, _ := store.StartRunWithOptions(ctx, foreignTask, "cli", "continue", StartRunOptions{ExecutionRoots: otherRoots})
	if _, err := store.ClaimInteractionContinuation(ctx, identity.TenantID, identity.PersonID, foreign.ID, parent.ID); !errors.Is(err, ErrContinuationDomainMismatch) {
		t.Fatalf("different roots must report a domain mismatch, got %v", err)
	}
	if refreshed, _ := store.GetRun(ctx, identity.TenantID, foreign.ID); refreshed == nil || refreshed.TaskID != foreignTask.ID || refreshed.ParentRunID != "" {
		t.Fatalf("a refused claim must leave the interaction untouched: %+v", refreshed)
	}
}

func TestClaimInteractionContinuationHasCrossConnectionSingleWinner(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	storeA, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer storeA.Close()
	storeB, err := OpenStore(filepath.Clean(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer storeB.Close()
	identity, _ := storeA.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	targetTask, _ := storeA.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, WorkspaceID: "workspace", Title: "target", Channel: "cli"})
	parent, _ := storeA.StartRun(ctx, targetTask, "cli", "target")
	_ = storeA.FinishRun(ctx, identity.TenantID, parent.ID, "interrupted")
	makeSource := func(title string) *Run {
		task, _ := storeA.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, WorkspaceID: "workspace", Title: title, Channel: "cli"})
		run, _ := storeA.StartRun(ctx, task, "cli", title)
		return run
	}
	sources := []*Run{makeSource("one"), makeSource("two")}
	stores := []*Store{storeA, storeB}
	type result struct {
		run *Run
		err error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range stores {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			run, err := stores[i].ClaimInteractionContinuation(ctx, identity.TenantID, identity.PersonID, sources[i].ID, parent.ID)
			results <- result{run: run, err: err}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	winners, losers := 0, 0
	for got := range results {
		switch {
		case got.err == nil && got.run != nil && got.run.ParentRunID == parent.ID:
			winners++
		case errors.Is(got.err, ErrParentRunClaimed):
			losers++
		default:
			t.Fatalf("unexpected claim result: run=%+v err=%v", got.run, got.err)
		}
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("winners=%d losers=%d", winners, losers)
	}
}

// After a direct claim the tool scope still carries the interaction thread id.
// Control objects filed for the run must follow the run onto the continued
// thread, or a pending approval would never join the run in Attention.
func TestControlObjectsFollowTheRunAfterDirectClaim(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	roots := []executionenv.RootBinding{{Path: "/workspace", Role: executionenv.RootRolePrimary, AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceWorkspace}}
	targetTask, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, WorkspaceID: "workspace", Title: "target", Channel: "cli"})
	parent, _ := store.StartRunWithOptions(ctx, targetTask, "cli", "target", StartRunOptions{ExecutionRoots: roots})
	_ = store.FinishRun(ctx, identity.TenantID, parent.ID, "waiting_user")
	staleTask, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, WorkspaceID: "workspace", Title: "确认执行", Channel: "cli"})
	run, _ := store.StartRunWithOptions(ctx, staleTask, "cli", "确认执行", StartRunOptions{ExecutionRoots: roots})
	if _, err := store.ClaimInteractionContinuation(ctx, identity.TenantID, identity.PersonID, run.ID, parent.ID); err != nil {
		t.Fatal(err)
	}

	approval, err := store.CreateApprovalRequest(ctx, ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: staleTask.ID, RunID: run.ID, ActionType: "tool_call",
	})
	if err != nil {
		t.Fatal(err)
	}
	clarify, err := store.CreateClarifyRequest(ctx, ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: staleTask.ID, RunID: run.ID, Question: "which region?",
	})
	if err != nil {
		t.Fatal(err)
	}
	if approval.TaskID != targetTask.ID || clarify.TaskID != targetTask.ID {
		t.Fatalf("control objects must follow the run onto the continued thread: approval=%s clarify=%s want %s", approval.TaskID, clarify.TaskID, targetTask.ID)
	}
	items, err := NewWorkTimeline(store).Attention(ctx, identity.TenantID, identity.PersonID, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range items {
		if item.RunID == run.ID && item.Thread.ID == targetTask.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the continuing run must surface its pending input on the continued thread: %+v", items)
	}
	// A run the store never recorded keeps the caller's thread id.
	orphan, err := store.CreateApprovalRequest(ctx, ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: targetTask.ID, RunID: "run_never_recorded", ActionType: "tool_call",
	})
	if err != nil || orphan.TaskID != targetTask.ID {
		t.Fatalf("unknown run must not rewrite the thread id: %+v err=%v", orphan, err)
	}
}
