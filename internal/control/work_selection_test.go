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
	_ = store.FinishRun(ctx, identity.TenantID, parent.ID, "interrupted")
	sourceTask, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, WorkspaceID: "workspace", Title: "continue target", Channel: "cli"})
	source, _ := store.StartRunWithOptions(ctx, sourceTask, "cli", "continue target", StartRunOptions{ExecutionRoots: roots})
	_, _ = store.AppendEvent(ctx, Event{TaskID: sourceTask.ID, RunID: source.ID, Type: "work.selection", Visibility: "task"})

	claimed, err := store.ClaimInteractionContinuation(ctx, identity.TenantID, identity.PersonID, source.ID, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.TaskID != targetTask.ID || claimed.ParentRunID != parent.ID {
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
	if refreshed == nil || refreshed.ActiveRunID != source.ID || refreshed.Status != "running" {
		t.Fatalf("target task projection = %+v", refreshed)
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

func TestEnqueueSelectedContinuationFreezesParentDomain(t *testing.T) {
	ctx := context.Background()
	store, identity, _, sourceRun := newRecoveryFixture(t)
	parentTask, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, WorkspaceID: "workspace-parent", Title: "parent", Channel: "cli"})
	parentRun, _ := store.StartRun(ctx, parentTask, "cli", "parent work")
	_ = store.FinishRun(ctx, identity.TenantID, parentRun.ID, "interrupted")
	queued, err := store.EnqueueSelectedContinuation(ctx, identity.TenantID, identity.PersonID, sourceRun.ID, parentRun.ID, QueuedTask{
		Platform: "weixin", PlatformUserID: "wx-user", Channel: "weixin", Content: "continue parent with this exact request",
	})
	if err != nil {
		t.Fatal(err)
	}
	if queued.TaskID != parentTask.ID || queued.ReplyToRunID != parentRun.ID || queued.WorkspaceID != parentRun.WorkspaceID {
		t.Fatalf("queued continuation = %+v", queued)
	}
	if queued.IdempotencyKey != "work-selection:"+sourceRun.ID+":"+parentRun.ID {
		t.Fatalf("idempotency key = %q", queued.IdempotencyKey)
	}
	retry, err := store.EnqueueSelectedContinuation(ctx, identity.TenantID, identity.PersonID, sourceRun.ID, parentRun.ID, QueuedTask{Content: "ignored retry"})
	if err != nil || retry.ID != queued.ID || retry.Content != queued.Content {
		t.Fatalf("retry = %+v err=%v", retry, err)
	}
}

func TestRunSelectionEffectBoundaryIgnoresReadOnlyAndLifecycle(t *testing.T) {
	ctx := context.Background()
	store, identity, _, run := newRecoveryFixture(t)
	for _, entry := range []ToolLedgerEntry{
		{RunID: run.ID, ToolCallID: "read", ToolName: "work_inspect", ArgsHash: "a", RetryClass: "read_only"},
		{RunID: run.ID, ToolCallID: "select", ToolName: "work_select", ArgsHash: "b", RetryClass: "idempotent"},
		{RunID: run.ID, ToolCallID: "plan", ToolName: "update_plan", ArgsHash: "c", RetryClass: "idempotent"},
	} {
		claim, err := store.ClaimToolDispatch(ctx, identity.TenantID, entry)
		if err != nil || !claim.Execute {
			t.Fatalf("claim %+v: %+v %v", entry, claim, err)
		}
		_ = store.RecordToolOutcome(ctx, identity.TenantID, run.ID, entry.ToolCallID, true)
	}
	blocked, reason, err := store.RunSelectionEffectBoundary(ctx, identity.TenantID, identity.PersonID, run.ID)
	if err != nil || blocked {
		t.Fatalf("safe selection boundary = %v %q err=%v", blocked, reason, err)
	}
	claim, err := store.ClaimToolDispatch(ctx, identity.TenantID, ToolLedgerEntry{
		RunID: run.ID, ToolCallID: "write", ToolName: "patch", ArgsHash: "d", RetryClass: "idempotent",
	})
	if err != nil || !claim.Execute {
		t.Fatalf("write claim: %+v %v", claim, err)
	}
	_ = store.RecordToolOutcome(ctx, identity.TenantID, run.ID, "write", true)
	blocked, reason, err = store.RunSelectionEffectBoundary(ctx, identity.TenantID, identity.PersonID, run.ID)
	if err != nil || !blocked || reason != "tool_effect" {
		t.Fatalf("write boundary = %v %q err=%v", blocked, reason, err)
	}
}

func TestProjectInteractionTaskHidesOnlySingleRunLabel(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	if err := store.ProjectInteractionTask(ctx, identity.TenantID, identity.PersonID, task.ID, run.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := store.GetTask(ctx, identity.TenantID, task.ID)
	if got == nil || got.Kind != "interaction" || got.Visibility != "hidden" {
		t.Fatalf("interaction task = %+v", got)
	}
	if current, _ := store.CurrentTask(ctx, identity.TenantID, identity.PersonID); current != nil && current.ID == task.ID {
		t.Fatalf("hidden interaction remained current: %+v", current)
	}
}
