package control

import (
	"context"
	"errors"
	"testing"
)

// newClarifyFixture builds a store with one identity and one task that has a
// live 'running' run — the state a run blocking on a question is in.
func newClarifyFixture(t *testing.T) (*Store, *IdentityContext, *Task, *Run) {
	t.Helper()
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "questioned task",
		Channel:  "cli",
	})
	if err != nil {
		t.Fatalf("task: %v", err)
	}
	run, err := store.StartRun(ctx, task, "cli", "do the work")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return store, identity, task, run
}

// TestClarifyCreateListAnswerRoundTrip is the create → list → answer contract:
// a pending question is listed for the person, then AnswerClarifyRequest
// finalizes it with the free-text answer and answering channel, and it no
// longer appears as pending.
func TestClarifyCreateListAnswerRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newClarifyFixture(t)

	created, err := store.CreateClarifyRequest(ctx, ClarifyRequest{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		TaskID:   task.ID,
		RunID:    run.ID,
		Question: "Which environment should I deploy to?",
		Channel:  "cli",
	})
	if err != nil {
		t.Fatalf("CreateClarifyRequest: %v", err)
	}
	if created.Status != "pending" || created.ID == "" {
		t.Fatalf("created = %+v", created)
	}

	pending, err := store.ListClarifyRequests(ctx, identity.TenantID, identity.PersonID, "pending", 10)
	if err != nil {
		t.Fatalf("ListClarifyRequests: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != created.ID {
		t.Fatalf("pending = %+v", pending)
	}

	answered, err := store.AnswerClarifyRequest(ctx, identity.TenantID, identity.PersonID, created.ID, "staging", "weixin")
	if err != nil {
		t.Fatalf("AnswerClarifyRequest: %v", err)
	}
	if answered.Status != "answered" || answered.Answer != "staging" || answered.Channel != "weixin" {
		t.Fatalf("answered = %+v", answered)
	}

	stillPending, err := store.ListClarifyRequests(ctx, identity.TenantID, identity.PersonID, "pending", 10)
	if err != nil {
		t.Fatalf("ListClarifyRequests: %v", err)
	}
	if len(stillPending) != 0 {
		t.Fatalf("still pending after answer: %+v", stillPending)
	}
}

// TestAnswerClarifyRejectsAlreadyAnswered proves a second answer to a resolved
// question is refused (the row is no longer pending), so a late reply can't
// clobber a recorded answer.
func TestAnswerClarifyRejectsAlreadyAnswered(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newClarifyFixture(t)
	created, err := store.CreateClarifyRequest(ctx, ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		Question: "pick one", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AnswerClarifyRequest(ctx, identity.TenantID, identity.PersonID, created.ID, "a", "cli"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AnswerClarifyRequest(ctx, identity.TenantID, identity.PersonID, created.ID, "b", "cli"); err == nil {
		t.Fatal("second answer to an already-answered question must fail")
	}
}

func TestAnswerParkedClarifyQueuesExactResumeOnce(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newClarifyFixture(t)
	created, err := store.CreateClarifyRequest(ctx, ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		Question: "pick a region", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "interrupted"); err != nil {
		t.Fatal(err)
	}

	answered, queued, err := store.AnswerClarifyRequestWithResume(ctx,
		identity.TenantID, identity.PersonID, created.ID, "us-east-1", "weixin",
		QueuedTask{Platform: "weixin", PlatformUserID: "wx-user"})
	if err != nil {
		t.Fatal(err)
	}
	if answered == nil || answered.Status != "answered" || answered.Answer != "us-east-1" {
		t.Fatalf("answered = %+v", answered)
	}
	if queued == nil || queued.ClarifyID != created.ID || queued.TaskID != task.ID || queued.Content != "us-east-1" {
		t.Fatalf("queued = %+v", queued)
	}

	if _, _, err := store.AnswerClarifyRequestWithResume(ctx,
		identity.TenantID, identity.PersonID, created.ID, "another answer", "cli", QueuedTask{}); err == nil {
		t.Fatal("a second answer must not create another continuation")
	}
	rows, err := store.ListQueued(ctx, identity.TenantID, identity.PersonID, QueueStatusQueued)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != queued.ID || rows[0].ClarifyID != created.ID {
		t.Fatalf("queued rows = %+v, want exactly one clarification continuation", rows)
	}
}

func TestAnswerParkedClarifyExpiresWhenOriginWasClaimed(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newClarifyFixture(t)
	created, err := store.CreateClarifyRequest(ctx, ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		Question: "pick a region", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartRunWithOptions(ctx, task, "cli", "another continuation", StartRunOptions{ParentRunID: run.ID}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := store.AnswerClarifyRequestWithResume(ctx,
		identity.TenantID, identity.PersonID, created.ID, "us-east-1", "cli", QueuedTask{}); !errors.Is(err, ErrClarifyOriginUnavailable) {
		t.Fatalf("answer error = %v, want ErrClarifyOriginUnavailable", err)
	}
	stored, err := store.GetClarifyRequest(ctx, identity.TenantID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.Status != "expired" {
		t.Fatalf("claimed-origin clarification = %+v, want expired", stored)
	}
	if rows, err := store.ListQueued(ctx, identity.TenantID, identity.PersonID, QueueStatusQueued); err != nil || len(rows) != 0 {
		t.Fatalf("stale answer must not queue: rows=%+v err=%v", rows, err)
	}
}

// TestExpireOrphanedClarifies proves the sweep preserves questions whose runs
// can still be resumed after a restart, while expiring a question attached to
// terminal work. A parked question is durable continuation state, not an
// orphan merely because its waiter process stopped.
func TestExpireOrphanedClarifies(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newClarifyFixture(t)

	// A second live run whose question must survive the sweep.
	liveRun, err := store.StartRun(ctx, task, "cli", "second run")
	if err != nil {
		t.Fatal(err)
	}

	parked, err := store.CreateClarifyRequest(ctx, ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		Question: "parked question", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	terminalRun, err := store.StartRun(ctx, task, "cli", "terminal run")
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := store.CreateClarifyRequest(ctx, ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: terminalRun.ID,
		Question: "stale terminal question", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimedRun, err := store.StartRun(ctx, task, "cli", "claimed parent")
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.CreateClarifyRequest(ctx, ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: claimedRun.ID,
		Question: "already continued question", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	live, err := store.CreateClarifyRequest(ctx, ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: liveRun.ID,
		Question: "live question", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Kill only the first run.
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, terminalRun.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, claimedRun.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartRunWithOptions(ctx, task, "cli", "claim it", StartRunOptions{ParentRunID: claimedRun.ID}); err != nil {
		t.Fatal(err)
	}

	n, err := store.ExpireOrphanedClarifies(ctx)
	if err != nil {
		t.Fatalf("ExpireOrphanedClarifies: %v", err)
	}
	if n != 2 {
		t.Fatalf("expired = %d, want 2", n)
	}
	gotParked, _ := store.GetClarifyRequest(ctx, identity.TenantID, parked.ID)
	if gotParked == nil || gotParked.Status != "pending" {
		t.Fatalf("parked = %+v, want pending", gotParked)
	}
	gotLive, _ := store.GetClarifyRequest(ctx, identity.TenantID, live.ID)
	if gotLive == nil || gotLive.Status != "pending" {
		t.Fatalf("live = %+v, want pending", gotLive)
	}
	gotTerminal, _ := store.GetClarifyRequest(ctx, identity.TenantID, terminal.ID)
	if gotTerminal == nil || gotTerminal.Status != "expired" {
		t.Fatalf("terminal = %+v, want expired", gotTerminal)
	}
	gotClaimed, _ := store.GetClarifyRequest(ctx, identity.TenantID, claimed.ID)
	if gotClaimed == nil || gotClaimed.Status != "expired" {
		t.Fatalf("claimed = %+v, want expired", gotClaimed)
	}
}

// TestMarkInterruptedRunsPreservesParkedClarify proves a boot sweep changes the
// run to resumable interrupted state without discarding the question needed to
// create its exact child continuation after the person answers.
func TestMarkInterruptedRunsPreservesParkedClarify(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newClarifyFixture(t)
	created, err := store.CreateClarifyRequest(ctx, ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		Question: "will be orphaned", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkInterruptedRuns(ctx, 0); err != nil {
		t.Fatalf("MarkInterruptedRuns: %v", err)
	}
	got, _ := store.GetClarifyRequest(ctx, identity.TenantID, created.ID)
	if got == nil || got.Status != "pending" {
		t.Fatalf("clarify after sweep = %+v, want pending", got)
	}
}
