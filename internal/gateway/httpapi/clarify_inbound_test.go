package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
	"selfmind/internal/runpool"
)

func newClarifyTestServer(t *testing.T) (*Server, *control.Store, *control.IdentityContext, *control.Task, *control.Run) {
	t.Helper()
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store := controltest.NewStore(t)
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

// TestClarifyMultiplePendingDisambiguates pins the P1 contract: an answer must
// never land on a question the person did not mean. Two pending questions get
// a deterministic numbered list; an "N: answer" prefix picks one exactly, and
// the other stays pending.
func TestClarifyMultiplePendingDisambiguates(t *testing.T) {
	daemon, store, identity, task, run := newClarifyTestServer(t)
	ctx := context.Background()
	first, err := store.CreateClarifyRequest(ctx, control.ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		Question: "Which environment?", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateClarifyRequest(ctx, control.ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		Question: "Which region?", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}

	handled, reply, err := daemon.tryHandleClarifyAnswer(ctx, identity, "", "staging please", "cli")
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if !strings.Contains(reply, "Several questions are waiting") || !strings.Contains(reply, "Which environment?") || !strings.Contains(reply, "Which region?") {
		t.Fatalf("ambiguous answer must list the pending questions, got %q", reply)
	}
	if got, _ := store.GetClarifyRequest(ctx, identity.TenantID, first.ID); got == nil || got.Status != "pending" {
		t.Fatalf("no question may be blindly claimed: %+v", got)
	}

	// Pick "Which region?" by the number the DISPLAYED list assigned it — the
	// person answers against that list, whatever internal order produced it.
	regionNumber := ""
	for _, line := range strings.Split(reply, "\n") {
		if strings.Contains(line, "Which region?") {
			regionNumber, _, _ = strings.Cut(line, ".")
			break
		}
	}
	if strings.TrimSpace(regionNumber) == "" {
		t.Fatalf("could not find the region question's number in %q", reply)
	}
	handled, reply, err = daemon.tryHandleClarifyAnswer(ctx, identity, "", regionNumber+": us-east-1", "cli")
	if err != nil || !handled || !strings.Contains(reply, "Got it") {
		t.Fatalf("numbered pick failed: handled=%v reply=%q err=%v", handled, reply, err)
	}
	if got, _ := store.GetClarifyRequest(ctx, identity.TenantID, second.ID); got == nil || got.Status != "answered" || got.Answer != "us-east-1" {
		t.Fatalf("picked question = %+v, want answered with us-east-1", got)
	}
	if got, _ := store.GetClarifyRequest(ctx, identity.TenantID, first.ID); got == nil || got.Status != "pending" {
		t.Fatalf("unpicked question must stay pending: %+v", got)
	}
}

func TestClarifyIDAnswersExactPendingQuestion(t *testing.T) {
	daemon, store, identity, task, run := newClarifyTestServer(t)
	ctx := context.Background()
	first, err := store.CreateClarifyRequest(ctx, control.ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		Question: "Which environment?", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateClarifyRequest(ctx, control.ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		Question: "Which region?", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli",
		Content: "us-east-1", ClarifyID: second.ID,
	})
	if status != http.StatusOK || !strings.Contains(resp.Content, "Got it") {
		t.Fatalf("exact clarify answer failed: status=%d resp=%+v", status, resp)
	}
	if got, _ := store.GetClarifyRequest(ctx, identity.TenantID, second.ID); got == nil || got.Status != "answered" || got.Answer != "us-east-1" {
		t.Fatalf("named clarification was not answered: %+v", got)
	}
	if got, _ := store.GetClarifyRequest(ctx, identity.TenantID, first.ID); got == nil || got.Status != "pending" {
		t.Fatalf("unrelated clarification changed: %+v", got)
	}
}

func TestParkedClarifyAnswerResumesExactOriginRun(t *testing.T) {
	daemon, store, identity, task, origin := newClarifyTestServer(t)
	ctx := context.Background()
	clarify, err := store.CreateClarifyRequest(ctx, control.ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: origin.ID,
		Question: "Which region?", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, origin.ID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	sibling := seedUnresolvedRun(t, store, identity.TenantID, task, "cli", "unrelated interrupted work", "interrupted")

	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli",
		Content: "us-east-1", ClarifyID: clarify.ID,
	})
	if status != http.StatusOK || !strings.Contains(resp.Content, "continuing") {
		t.Fatalf("parked clarification answer failed: status=%d resp=%+v", status, resp)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runs, listErr := store.ListTaskRuns(ctx, identity.TenantID, task.ID, 20)
		if listErr != nil {
			t.Fatal(listErr)
		}
		for _, run := range runs {
			if run.ResumesRunID == sibling.ID {
				t.Fatalf("clarification resumed the sibling run: %+v", run)
			}
			if run.ResumesRunID == origin.ID {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("answering a parked clarification did not create a child run for its exact origin")
}

func TestParkedClarifyAnswerDoesNotFollowAClaimedOrigin(t *testing.T) {
	daemon, store, identity, task, origin := newClarifyTestServer(t)
	ctx := context.Background()
	clarify, err := store.CreateClarifyRequest(ctx, control.ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: origin.ID,
		Question: "Which region?", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, origin.ID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartRunWithOptions(ctx, task, "cli", "already continuing", control.StartRunOptions{ResumesRunID: origin.ID}); err != nil {
		t.Fatal(err)
	}

	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli",
		Content: "us-east-1", ClarifyID: clarify.ID,
	})
	if status != http.StatusOK || !strings.Contains(resp.Content, "no longer waiting") {
		t.Fatalf("stale clarification response: status=%d resp=%+v", status, resp)
	}
	stored, err := store.GetClarifyRequest(ctx, identity.TenantID, clarify.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.Status != "expired" {
		t.Fatalf("stale clarification = %+v, want expired", stored)
	}
}

// TestClarifyAnswerIgnoredWithNoPending: a plain message with no question
// pending must NOT be claimed — it flows on to normal routing.
func TestClarifyAnswerIgnoredWithNoPending(t *testing.T) {
	daemon, _, identity, _, _ := newClarifyTestServer(t)
	handled, _, _ := daemon.tryHandleClarifyAnswer(context.Background(), identity, "", "hello there", "cli")
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
	handled, _, _ := daemon.tryHandleClarifyAnswer(ctx, identity, "", "/status", "cli")
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

func TestGatewayClarifyPausesRunWatchdog(t *testing.T) {
	daemon, store, identity, task, run := newClarifyTestServer(t)
	runCtx, _, stop := runpool.WithWatchdog(context.Background(), 50*time.Millisecond)
	defer stop()
	handler := daemon.coordinator().gatewayClarify(runCtx, identity, task, run, "cli")

	resultCh := make(chan string, 1)
	go func() { resultCh <- handler("Which environment?", []string{"staging", "prod"}) }()
	clarify := waitForPendingClarify(t, store, identity)
	time.Sleep(125 * time.Millisecond)
	if err := runCtx.Err(); err != nil {
		t.Fatalf("watchdog fired while waiting for a person: %v", err)
	}
	if _, err := store.AnswerClarifyRequest(context.Background(), identity.TenantID, identity.PersonID, clarify.ID, "prod", "weixin"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-resultCh:
		if got != "prod" {
			t.Fatalf("clarify result = %q, want prod", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("clarify waiter did not resume")
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

// TestGatewayClarifyPreservesQuestionOnGatewayShutdown separates a daemon
// restart from an ordinary user cancellation. The waiter exits in both cases,
// but restart recovery needs the pending row so a later answer can resume the
// exact origin run through its ClarifyID.
func TestGatewayClarifyPreservesQuestionOnGatewayShutdown(t *testing.T) {
	daemon, store, identity, task, run := newClarifyTestServer(t)
	runCtx, cancel := context.WithCancelCause(context.Background())
	handler := daemon.coordinator().gatewayClarify(runCtx, identity, task, run, "cli")

	resultCh := make(chan string, 1)
	go func() { resultCh <- handler("Which environment?", nil) }()
	clarify := waitForPendingClarify(t, store, identity)
	cancel(errGatewayShutdown)

	select {
	case got := <-resultCh:
		if got != clarifyFallbackSentinel {
			t.Fatalf("clarify result = %q, want fallback sentinel", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gatewayClarify ignored gateway shutdown")
	}
	stored, err := store.GetClarifyRequest(context.Background(), identity.TenantID, clarify.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.Status != "pending" {
		t.Fatalf("clarify after gateway shutdown = %+v, want pending", stored)
	}
}

// TestStatusShowsPendingClarify: /status surfaces the pending question so a run
// blocked on a clarify does not look silently stuck.
func TestStatusShowsPendingClarify(t *testing.T) {
	daemon, store, identity, task, run := newClarifyTestServer(t)
	ctx := context.Background()
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

// While the terminal that asked a question is open, only it answers with plain
// text: a message typed in another terminal is that terminal's own input, and
// used to be taken as the answer. There the question takes the named form
// "N: answer". Once the asking terminal is gone, any plain reply answers again.
func TestPlainRepliesAnswerOnlyTheAskingSessionWhileItIsOpen(t *testing.T) {
	daemon, store, identity, task, run := newClarifyTestServer(t)
	ctx := context.Background()
	clarify, err := store.CreateClarifyRequest(ctx, control.ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		Question: "Which bucket?", Channel: "session-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	pending := func() bool {
		current, _ := store.GetClarifyRequest(ctx, identity.TenantID, clarify.ID)
		return current != nil && current.Status == "pending"
	}
	_, closeA := daemon.events().subscribe(identity.PersonID, "session-a", "")

	if handled, _, err := daemon.tryHandleClarifyAnswer(ctx, identity, "", "refactor the parser next", "session-b"); err != nil || handled || !pending() {
		t.Fatalf("another open session's plain message answered the question: handled=%v err=%v", handled, err)
	}
	if handled, reply, err := daemon.tryHandleClarifyAnswer(ctx, identity, "", "1: the staging bucket", "session-b"); err != nil || !handled || pending() || !strings.Contains(reply, "Got it") {
		t.Fatalf("the named form did not answer from another session: handled=%v reply=%q err=%v", handled, reply, err)
	}
	if current, _ := store.GetClarifyRequest(ctx, identity.TenantID, clarify.ID); current == nil || current.Answer != "the staging bucket" {
		t.Fatalf("recorded answer = %+v", current)
	}

	second, err := store.CreateClarifyRequest(ctx, control.ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		Question: "Which region?", Channel: "session-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	closeA()
	if handled, _, err := daemon.tryHandleClarifyAnswer(ctx, identity, "", "eu-west-1", "session-b"); err != nil || !handled {
		t.Fatalf("with the asking terminal closed a plain reply must answer: handled=%v err=%v", handled, err)
	}
	if current, _ := store.GetClarifyRequest(ctx, identity.TenantID, second.ID); current == nil || current.Answer != "eu-west-1" {
		t.Fatalf("recorded answer = %+v", current)
	}
}

// While another open session also has a question, a numbered reply is read
// against the whole list, the way a listing numbers it, and a time, a decimal,
// or a number that starts a plain answer stays the answer. "1. staging" used to
// land on this session's question with its prefix, even when 1 named the other
// session's question, and "1:30" named question 1.
func TestNumberedRepliesCountTheWholeListWhileAnotherSessionAsks(t *testing.T) {
	daemon, store, identity, task, run := newClarifyTestServer(t)
	ctx := context.Background()
	theirs, err := store.CreateClarifyRequest(ctx, control.ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		Question: "Which bucket?", Channel: "session-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, closeA := daemon.events().subscribe(identity.PersonID, "session-a", "")
	defer closeA()
	status := func(id string) (string, string) {
		current, _ := store.GetClarifyRequest(ctx, identity.TenantID, id)
		return current.Status, current.Answer
	}
	ask := func() string {
		t.Helper()
		ours, err := store.CreateClarifyRequest(ctx, control.ClarifyRequest{
			TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
			Question: "How long should the canary run?", Channel: "session-b",
		})
		if err != nil {
			t.Fatal(err)
		}
		return ours.ID
	}

	// The listing numbers questions oldest first with an id tie-break, and
	// both questions may share a second, so take each number from that order.
	number := func(id string) int {
		t.Helper()
		pending, err := store.ListClarifyRequests(ctx, identity.TenantID, identity.PersonID, "pending", 100)
		if err != nil {
			t.Fatal(err)
		}
		sort.SliceStable(pending, func(i, j int) bool {
			if pending[i].CreatedAt.Equal(pending[j].CreatedAt) {
				return pending[i].ID < pending[j].ID
			}
			return pending[i].CreatedAt.Before(pending[j].CreatedAt)
		})
		for i, clarify := range pending {
			if clarify.ID == id {
				return i + 1
			}
		}
		t.Fatalf("question %s is not pending", id)
		return 0
	}

	ours := ask()
	pick := fmt.Sprintf("%d. staging", number(theirs.ID))
	handled, reply, err := daemon.tryHandleClarifyAnswer(ctx, identity, "", pick, "session-b")
	if s, _ := status(ours); err != nil || !handled || s != "pending" || !strings.Contains(reply, "Several questions are waiting") {
		t.Fatalf("%q names the other session's question: handled=%v reply=%q ours=%s err=%v", pick, handled, reply, s, err)
	}
	if s, _ := status(theirs.ID); s != "pending" {
		t.Fatalf("the other session's question was answered by a lax prefix: %s", s)
	}
	pick = fmt.Sprintf("%d. staging", number(ours))
	if handled, _, err := daemon.tryHandleClarifyAnswer(ctx, identity, "", pick, "session-b"); err != nil || !handled {
		t.Fatalf("%q: handled=%v err=%v", pick, handled, err)
	}
	if s, answer := status(ours); s == "pending" || answer != "staging" {
		t.Fatalf("%q recorded status=%s answer=%q, want this session's question answered with the prefix removed", pick, s, answer)
	}
	for _, plain := range []string{"2 buckets", "1:30", "1.5"} {
		ours := ask()
		if handled, _, err := daemon.tryHandleClarifyAnswer(ctx, identity, "", plain, "session-b"); err != nil || !handled {
			t.Fatalf("%q: handled=%v err=%v", plain, handled, err)
		}
		if s, answer := status(ours); s == "pending" || answer != plain {
			t.Fatalf("%q recorded status=%s answer=%q, want it as this session's answer", plain, s, answer)
		}
		if s, _ := status(theirs.ID); s != "pending" {
			t.Fatalf("%q answered the other session's question", plain)
		}
	}
}
