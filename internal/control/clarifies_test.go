package control

import (
	"context"
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

// TestExpireOrphanedClarifies proves the sweep expires a pending question whose
// run is no longer running (daemon killed mid-wait) but leaves a question whose
// run is still live untouched — the same contract as ExpireOrphanedApprovals.
func TestExpireOrphanedClarifies(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newClarifyFixture(t)

	// A second live run whose question must survive the sweep.
	liveRun, err := store.StartRun(ctx, task, "cli", "second run")
	if err != nil {
		t.Fatal(err)
	}

	orphan, err := store.CreateClarifyRequest(ctx, ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		Question: "orphan question", Channel: "cli",
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

	n, err := store.ExpireOrphanedClarifies(ctx)
	if err != nil {
		t.Fatalf("ExpireOrphanedClarifies: %v", err)
	}
	if n != 1 {
		t.Fatalf("expired = %d, want 1", n)
	}
	gotOrphan, _ := store.GetClarifyRequest(ctx, identity.TenantID, orphan.ID)
	if gotOrphan == nil || gotOrphan.Status != "expired" {
		t.Fatalf("orphan = %+v, want expired", gotOrphan)
	}
	gotLive, _ := store.GetClarifyRequest(ctx, identity.TenantID, live.ID)
	if gotLive == nil || gotLive.Status != "pending" {
		t.Fatalf("live = %+v, want pending", gotLive)
	}
}

// TestMarkInterruptedRunsExpiresOrphanedClarify proves the recovery sweep
// (boot + periodic) also expires a dangling question, so a restart never leaves
// a run blocked on a clarify_requests row whose run is gone.
func TestMarkInterruptedRunsExpiresOrphanedClarify(t *testing.T) {
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
	if got == nil || got.Status != "expired" {
		t.Fatalf("clarify after sweep = %+v, want expired", got)
	}
}
