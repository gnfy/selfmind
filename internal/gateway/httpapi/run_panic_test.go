package httpapi

// P0/P1-4: an async run goroutine has no net/http per-request recover, so an
// unrecovered panic in runMessage would crash the whole gateway daemon. The
// coordinator wraps the goroutine body in a recover that logs, finalizes the
// run failed + task interrupted, and frees the person's slot. This test drives a
// provider that panics synchronously on the run goroutine and asserts the daemon
// survives and nothing is left wedged.

import (
	"context"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/gateway/router"
)

// panicLLMProvider panics on the streaming path, synchronously on the caller's
// goroutine (streamChatWithRetry calls StreamChat directly), so the panic
// propagates up the async run goroutine to the coordinator's recover.
type panicLLMProvider struct{}

func (panicLLMProvider) ChatCompletion(ctx context.Context, messages []llm.Message) (string, error) {
	panic("boom: ChatCompletion")
}
func (panicLLMProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	panic("boom: Chat")
}
func (panicLLMProvider) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	panic("boom: StreamChat in async run")
}

func TestAsyncRunPanicIsRecovered(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	agent := kernel.NewAgent(memory.NewMemoryManager(nil), stubToolBackend{}, panicLLMProvider{}, "test agent", 1, 1, nil)
	gw := router.NewGateway(nil, nil, agent, nil)
	daemon := &Server{Control: store, Gateway: gw, DefaultTenantID: "default"}
	sender := &syncedSender{}
	daemon.Delivery = delivery.NewService(store, sender, delivery.Options{})

	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}

	resp, _ := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "explode please", Async: true,
	})
	if !resp.Accepted {
		t.Fatalf("async run should be accepted before it panics: %+v", resp)
	}

	// If the panic had crashed the daemon this test process would be gone; the
	// fact that we get here and can observe recovery is the survival proof.
	// The run must reach a terminal state (no running run left).
	waitUntil(t, 5*time.Second, func() bool {
		runs, rerr := store.ListRunningRuns(ctx, identity.TenantID, []string{identity.PersonID})
		return rerr == nil && len(runs) == 0
	}, "panicked run left a running run (daemon likely wedged)")

	// The active-run registry slot is freed, so the person is not wedged.
	waitUntil(t, 5*time.Second, func() bool {
		return daemon.coordinator().currentActive(identity.PersonID) == nil
	}, "person slot never freed after a panic")

	// The task parked non-terminal (interrupted), never left 'running'.
	task, err := store.CurrentTask(ctx, identity.TenantID, identity.PersonID)
	if err != nil || task == nil {
		t.Fatalf("current task: %v %v", task, err)
	}
	if task.Status == "running" {
		t.Fatalf("task must not be left 'running' after a panic; status=%q", task.Status)
	}

	// A fresh message is accepted (not rejected as busy) — the daemon keeps serving.
	next, _ := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "still alive?", Async: true,
	})
	if next.Turn != nil && next.Turn.Status == "busy" {
		t.Fatalf("person wedged after panic: next message rejected busy")
	}
	// Let the second (also panicking) run finalize before teardown.
	waitUntil(t, 5*time.Second, func() bool {
		return daemon.coordinator().currentActive(identity.PersonID) == nil
	}, "second run slot never freed")
}
