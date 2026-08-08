package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

func newClarifyTestServer(t *testing.T) (*Server, *control.Store, *control.IdentityContext, *control.Task, *control.Run) {
	t.Helper()
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Deploy service", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "do the work")
	if err != nil {
		t.Fatal(err)
	}
	return &Server{Control: store, DefaultTenantID: "default"}, store, identity, task, run
}

// TestClarifyAnswerResolvesPendingQuestion is the core inbound contract: with a
// question pending, a plain (non-slash) reply is recorded as the answer and
// confirmed, instead of being queued or dispatched.
func TestClarifyAnswerResolvesPendingQuestion(t *testing.T) {
	daemon, store, identity, task, run := newClarifyTestServer(t)
	ctx := context.Background()
	clarify, err := store.CreateClarifyRequest(ctx, control.ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		Question: "Which environment?", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{Content: "staging please"})
	if status != http.StatusOK {
		t.Fatalf("status = %d resp = %+v", status, resp)
	}
	if !strings.Contains(resp.Content, "Got it") {
		t.Fatalf("content = %q", resp.Content)
	}
	got, _ := store.GetClarifyRequest(ctx, identity.TenantID, clarify.ID)
	if got == nil || got.Status != "answered" || got.Answer != "staging please" {
		t.Fatalf("clarify = %+v", got)
	}
}

// TestClarifyAnswerIgnoredWithNoPending: a plain message with no question
// pending must NOT be claimed — it flows on to normal routing.
func TestClarifyAnswerIgnoredWithNoPending(t *testing.T) {
	daemon, _, identity, _, _ := newClarifyTestServer(t)
	handled, _, _ := daemon.tryHandleClarifyAnswer(context.Background(), identity, "hello there", "cli")
	if handled {
		t.Fatal("free text with no pending question must fall through")
	}
}

// TestClarifySlashNotTreatedAsAnswer: while a question is pending, a slash
// command is still a command (e.g. /status), never the answer.
func TestClarifySlashNotTreatedAsAnswer(t *testing.T) {
	daemon, store, identity, task, run := newClarifyTestServer(t)
	ctx := context.Background()
	if _, err := store.CreateClarifyRequest(ctx, control.ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		Question: "Which environment?", Channel: "cli",
	}); err != nil {
		t.Fatal(err)
	}
	handled, _, _ := daemon.tryHandleClarifyAnswer(ctx, identity, "/status", "cli")
	if handled {
		t.Fatal("a slash command must not be treated as a clarify answer")
	}
}

// TestGatewayClarifyAnswerRoundTrip drives the blocking waiter end to end: the
// handler creates the pending row and blocks; an answer recorded from another
// endpoint is returned verbatim as the tool result.
func TestGatewayClarifyAnswerRoundTrip(t *testing.T) {
	daemon, store, identity, task, run := newClarifyTestServer(t)
	handler := daemon.coordinator().gatewayClarify(context.Background(), identity, task, run, "cli")

	resultCh := make(chan string, 1)
	go func() { resultCh <- handler("Which environment?", []string{"staging", "prod"}) }()

	clarify := waitForPendingClarify(t, store, identity)
	if _, err := store.AnswerClarifyRequest(context.Background(), identity.TenantID, identity.PersonID, clarify.ID, "prod", "weixin"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-resultCh:
		if got != "prod" {
			t.Fatalf("clarify result = %q, want prod", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gatewayClarify did not return after the answer was recorded")
	}
}

// TestGatewayClarifyExpireFallsBack proves the unanswered path is safe: when the
// pending row is expired (the timeout / orphan-sweep outcome), the handler
// returns the best-judgment sentinel instead of hanging.
func TestGatewayClarifyExpireFallsBack(t *testing.T) {
	daemon, store, identity, task, run := newClarifyTestServer(t)
	handler := daemon.coordinator().gatewayClarify(context.Background(), identity, task, run, "cli")

	resultCh := make(chan string, 1)
	go func() { resultCh <- handler("Which environment?", nil) }()

	clarify := waitForPendingClarify(t, store, identity)
	if err := store.ExpireClarifyRequest(context.Background(), identity.TenantID, clarify.ID, "test timeout"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-resultCh:
		if got != clarifyFallbackSentinel {
			t.Fatalf("clarify result = %q, want fallback sentinel", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gatewayClarify did not fall back after the question expired")
	}
}

func TestGatewayClarifyStopsWhenRunIsCancelled(t *testing.T) {
	daemon, store, identity, task, run := newClarifyTestServer(t)
	runCtx, cancel := context.WithCancel(context.Background())
	handler := daemon.coordinator().gatewayClarify(runCtx, identity, task, run, "cli")

	resultCh := make(chan string, 1)
	go func() { resultCh <- handler("Which environment?", nil) }()
	clarify := waitForPendingClarify(t, store, identity)
	cancel()

	select {
	case got := <-resultCh:
		if got != clarifyFallbackSentinel {
			t.Fatalf("clarify result = %q, want fallback sentinel", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gatewayClarify ignored run cancellation")
	}
	stored, err := store.GetClarifyRequest(context.Background(), identity.TenantID, clarify.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.Status != "expired" {
		t.Fatalf("clarify after cancellation = %+v, want expired", stored)
	}
}

// TestStatusShowsPendingClarify: /status surfaces the pending question so a run
// blocked on a clarify does not look silently stuck.
func TestStatusShowsPendingClarify(t *testing.T) {
	daemon, store, identity, task, run := newClarifyTestServer(t)
	ctx := context.Background()
	if err := store.SetCurrentTask(ctx, identity.TenantID, identity.PersonID, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateClarifyRequest(ctx, control.ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		Question: "Which environment should I deploy to?", Channel: "cli",
	}); err != nil {
		t.Fatal(err)
	}
	reply, err := daemon.statusReply(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "Waiting for your answer") || !strings.Contains(reply, "elapsed)") || !strings.Contains(reply, "Which environment") {
		t.Fatalf("status = %q", reply)
	}
}

// TestDigestIncludesPendingClarify: the attach digest reports a pending question
// so reopening the CLI shows the run is waiting on the person.
func TestDigestIncludesPendingClarify(t *testing.T) {
	daemon, store, identity, task, run := newClarifyTestServer(t)
	ctx := context.Background()
	if _, err := store.CreateClarifyRequest(ctx, control.ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		Question: "Which environment?", Channel: "cli",
	}); err != nil {
		t.Fatal(err)
	}
	digest, err := daemon.buildDigest(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	if len(digest.PendingClarifies) != 1 || !strings.Contains(digest.PendingClarifies[0].Line, "Which environment") {
		t.Fatalf("digest clarifies = %+v", digest.PendingClarifies)
	}
	if digest.Empty() {
		t.Fatal("digest with a pending clarify must not be Empty()")
	}
}

func waitForPendingClarify(t *testing.T, store *control.Store, identity *control.IdentityContext) control.ClarifyRequest {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pending, err := store.ListClarifyRequests(context.Background(), identity.TenantID, identity.PersonID, "pending", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) > 0 {
			return pending[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no pending clarify was created")
	return control.ClarifyRequest{}
}
