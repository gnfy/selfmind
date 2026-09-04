package httpapi

// G0-a detached run execution: runs belong to the gateway, endpoints only
// watch. These tests pin the invariant that severing the dispatching HTTP
// connection mid-turn detaches a watcher (the run finishes and its result is
// routed like an async one), while cancellation still works exclusively
// through the active-run registry (/stop).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

// slowLLMProvider is a scripted provider whose StreamChat blocks until the
// test releases it (or the run ctx is cancelled), simulating a long turn.
type slowLLMProvider struct {
	answer      string
	started     chan struct{} // closed on the first StreamChat call
	release     chan struct{} // closed by the test to let the turn finish
	startOnce   sync.Once
	releaseOnce sync.Once
}

func newSlowLLMProvider(answer string) *slowLLMProvider {
	return &slowLLMProvider{
		answer:  answer,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (p *slowLLMProvider) releaseNow() {
	p.releaseOnce.Do(func() { close(p.release) })
}

func (p *slowLLMProvider) ChatCompletion(ctx context.Context, messages []llm.Message) (string, error) {
	return p.answer, nil
}

func (p *slowLLMProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Content: p.answer}, nil
}

func (p *slowLLMProvider) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.startOnce.Do(func() { close(p.started) })
	out := make(chan llm.StreamEvent, 2)
	go func() {
		defer close(out)
		select {
		case <-p.release:
			out <- llm.StreamEvent{Content: p.answer}
		case <-ctx.Done():
			out <- llm.StreamEvent{Err: ctx.Err()}
		}
	}()
	return out, nil
}

// stubToolBackend satisfies kernel.AgentBackend with no tools.
type stubToolBackend struct{}

func (stubToolBackend) Dispatch(name string, args map[string]interface{}) (string, error) {
	return "ok", nil
}
func (stubToolBackend) GetToolDefinitions() []map[string]interface{} { return nil }

// syncedSender is a mutex-guarded recording delivery sender: detached-run
// fan-out happens on the daemon-side handler goroutine after the client is
// gone, so assertions read it from a different goroutine than the writes.
type syncedSender struct {
	mu       sync.Mutex
	messages []delivery.Message
}

func (s *syncedSender) Send(ctx context.Context, msg delivery.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, msg)
	return nil
}

func (s *syncedSender) snapshot() []delivery.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]delivery.Message(nil), s.messages...)
}

// newDetachedRunServer builds a Server with a real router.Gateway around a
// scripted slow provider, so ProcessMessage exercises the genuine
// RunAgentWithEvents streaming path.
func newDetachedRunServer(t *testing.T, provider *slowLLMProvider) (*Server, *control.Store, *syncedSender) {
	t.Helper()
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	t.Cleanup(provider.releaseNow)

	agent := kernel.NewAgent(memory.NewMemoryManager(nil), stubToolBackend{}, provider, "test agent", 1, 1, nil)
	// nil intent llmProvider: intent stays rule-based so the blocking provider
	// is only reached by the actual agent turn.
	gw := router.NewGateway(agent, nil)
	daemon := &Server{Control: store, Gateway: gw, DefaultTenantID: "default"}
	sender := &syncedSender{}
	daemon.Delivery = delivery.NewService(store, sender, delivery.Options{})
	return daemon, store, sender
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSyncRunSurvivesClientDisconnect is the G0-a acceptance test: killing the
// HTTP connection mid-turn (closing the CLI) must NOT kill the run. The run
// finishes on the daemon-owned ctx, the task ends terminal/non-interrupted,
// and the result is delivered to the person's preferred IM endpoint (single
// target, G0-b) because the sync response had no reader left.
func TestSyncRunSurvivesClientDisconnect(t *testing.T) {
	provider := newSlowLLMProvider("Task completed successfully.")
	daemon, store, sender := newDetachedRunServer(t, provider)
	ctx := context.Background()

	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wxid_detached", "Me on WeChat"); err != nil {
		t.Fatalf("bind: %v", err)
	}

	ts := httptest.NewServer(daemon.Handler())
	defer ts.Close()

	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	body := `{"platform":"cli","platform_user_id":"local","channel":"cli","content":"do the slow thing"}`
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, ts.URL+"/v1/message", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	clientErr := make(chan error, 1)
	go func() {
		resp, err := ts.Client().Do(httpReq)
		if resp != nil {
			resp.Body.Close()
		}
		clientErr <- err
	}()

	select {
	case <-provider.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the agent turn never reached the provider")
	}
	// The client vanishes mid-run.
	cancelReq()
	if err := <-clientErr; err == nil {
		t.Fatal("expected the aborted client request to error")
	}
	// The client-side abort confirms the CLIENT gave up, but the SERVER
	// notices the dropped connection asynchronously (net/http's connection
	// reader cancels r.Context() after processing the FIN). The
	// detached-finish check runs once at run completion, so let the
	// cancellation propagate BEFORE releasing the run — otherwise completion
	// can win the race and classify the turn as still-attached (observed as a
	// macOS-runner flake in CI; Linux consistently won the race the other
	// way). The same few-ms window exists in production and is an accepted
	// residual: the result still lands in the run outcome/handoff, only the
	// push is skipped.
	time.Sleep(500 * time.Millisecond)
	// The run must still be executing on the daemon (detach, not cancel).
	provider.releaseNow()

	// The detached result is routed like an async one: to the single
	// preferred IM endpoint (the only bound account here).
	waitUntil(t, 10*time.Second, func() bool { return len(sender.snapshot()) > 0 },
		"detached sync result was not delivered to the preferred IM endpoint")
	msg := sender.snapshot()[0]
	if msg.Platform != "weixin" || msg.PlatformUserID != "wxid_detached" {
		t.Fatalf("delivery target = %s/%s, want weixin/wxid_detached", msg.Platform, msg.PlatformUserID)
	}
	if !strings.Contains(msg.Content, "Task completed successfully") {
		t.Fatalf("delivery content = %q, want the final answer", msg.Content)
	}

	// The run reached a terminal state and the task is not interrupted.
	waitUntil(t, 10*time.Second, func() bool {
		runs, err := store.ListRunningRuns(ctx, identity.TenantID, []string{identity.PersonID})
		return err == nil && len(runs) == 0
	}, "run did not reach a terminal status")
	threads, err := control.NewWorkTimeline(store).Search(ctx, identity.TenantID, identity.PersonID, "slow thing", 10)
	if err != nil || len(threads) != 1 {
		t.Fatalf("completed interaction history: %+v %v", threads, err)
	}
	task, err := store.GetTask(ctx, identity.TenantID, threads[0].ID)
	if err != nil || task == nil {
		t.Fatalf("completed interaction projection: %+v %v", task, err)
	}
	switch task.Status {
	case "interrupted", "cancelled", "failed", "blocked", "running":
		t.Fatalf("task status = %q; a detached run must finish normally", task.Status)
	}
}

// TestConnectedClientSyncRunDoesNotFanOut asserts the flip side: a client that
// stays connected gets the synchronous answer and nothing is pushed to IM.
func TestConnectedClientSyncRunDoesNotFanOut(t *testing.T) {
	provider := newSlowLLMProvider("Task completed successfully.")
	provider.releaseNow() // fast turn: no need to block
	daemon, store, sender := newDetachedRunServer(t, provider)
	ctx := context.Background()

	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wxid_conn", "Me on WeChat"); err != nil {
		t.Fatalf("bind: %v", err)
	}

	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform:       "cli",
		PlatformUserID: "local",
		Channel:        "cli",
		Content:        "do the quick thing",
	})
	if status != http.StatusOK || resp.Error != "" {
		t.Fatalf("sync run failed: status=%d resp=%+v", status, resp)
	}
	if !strings.Contains(resp.Content, "Task completed successfully") {
		t.Fatalf("sync answer = %q", resp.Content)
	}
	if got := sender.snapshot(); len(got) != 0 {
		t.Fatalf("connected sync run must not push to IM, got %+v", got)
	}
}

// TestStopStillCancelsDetachedRun proves cancellation ownership: with the run
// detached from the request ctx, /stop (the registry path) is what cancels it.
func TestStopStillCancelsDetachedRun(t *testing.T) {
	provider := newSlowLLMProvider("never finishes on its own")
	daemon, store, sender := newDetachedRunServer(t, provider)
	ctx := context.Background()

	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}

	type result struct {
		resp   api.MessageResponse
		status int
	}
	done := make(chan result, 1)
	go func() {
		resp, status := daemon.ProcessMessage(context.Background(), api.MessageRequest{
			Platform:       "cli",
			PlatformUserID: "local",
			Channel:        "cli",
			Content:        "run forever",
		})
		done <- result{resp, status}
	}()

	select {
	case <-provider.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the agent turn never reached the provider")
	}

	stopResp, stopStatus := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform:       "cli",
		PlatformUserID: "local",
		Channel:        "cli",
		Content:        "/stop",
	})
	if stopStatus != http.StatusOK || !strings.Contains(stopResp.Content, "Stopping run") {
		t.Fatalf("/stop reply: status=%d resp=%+v", stopStatus, stopResp)
	}

	var got result
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("/stop did not cancel the detached run")
	}
	if got.resp.Turn == nil || got.resp.Turn.Status != "cancelled" {
		t.Fatalf("turn = %+v, want cancelled", got.resp.Turn)
	}
	if got.resp.Run == nil {
		t.Fatalf("cancelled response lost its Run: %+v", got.resp)
	}
	storedRun, err := store.GetRun(ctx, identity.TenantID, got.resp.Run.ID)
	if err != nil || storedRun == nil || storedRun.Status != "cancelled" {
		t.Fatalf("authoritative run = %+v err=%v, want cancelled", storedRun, err)
	}
	runs, err := store.ListRunningRuns(ctx, identity.TenantID, []string{identity.PersonID})
	if err != nil || len(runs) != 0 {
		t.Fatalf("running runs after /stop = %v (%v)", runs, err)
	}
	// A user-cancelled run pushes nothing, connected or not.
	if got := sender.snapshot(); len(got) != 0 {
		t.Fatalf("cancelled run must not push to IM, got %+v", got)
	}
}

// TestSyncRunKeepsCallerDeadline pins the deadline carry-over: a deadline on
// the request ctx is a bound on the RUN (eval turn budgets), so detaching from
// connection-cancellation must not drop it.
func TestSyncRunKeepsCallerDeadline(t *testing.T) {
	provider := newSlowLLMProvider("never finishes on its own")
	daemon, _, _ := newDetachedRunServer(t, provider)

	turnCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	resp, status := daemon.ProcessMessage(turnCtx, api.MessageRequest{
		Platform:       "cli",
		PlatformUserID: "local",
		Channel:        "cli",
		Content:        "run past the deadline",
	})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("deadline was not enforced on the detached run (took %s)", elapsed)
	}
	if status != http.StatusOK || resp.Turn == nil || resp.Turn.Status != "cancelled" {
		t.Fatalf("deadline turn: status=%d resp=%+v", status, resp)
	}
}
