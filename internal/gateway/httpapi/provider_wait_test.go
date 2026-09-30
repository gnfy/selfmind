package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

func TestProviderCapacityParksExactRunAndReleasesAgent(t *testing.T) {
	provider := newSlowLLMProvider("done")
	defer provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	gate := llm.NewRequestGate(1)
	wrapped := gate.Wrap(provider, "one-route")
	agents := make([]*kernel.Agent, 2)
	for i := range agents {
		agents[i] = kernel.NewAgent(memory.NewMemoryManager(nil), stubToolBackend{}, wrapped, "test", 1, 1, nil)
	}
	daemon.Gateway = router.NewGateway(agents[0], nil)
	daemon.Gateway.EnableWorkerPool(agents[1:])
	daemon.coordinator().activeLimit = 2
	ctx := context.Background()
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	workspace := func(name string) string {
		t.Helper()
		root := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		ws, err := store.EnsureWorkspace(ctx, control.Workspace{TenantID: owner.TenantID,
			OwnerPersonID: owner.PersonID, Name: name, LocalPath: root})
		if err != nil {
			t.Fatal(err)
		}
		return ws.ID
	}
	first, code := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local",
		Channel: "session-a", WorkspaceID: workspace("a"), Content: "first independent work", Async: true})
	if code != 200 || !first.Accepted {
		t.Fatalf("first admission: %d %+v", code, first)
	}
	select {
	case <-provider.started:
	case <-time.After(3 * time.Second):
		t.Fatal("first model call did not hold route")
	}
	second, code := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local",
		Channel: "session-b", WorkspaceID: workspace("b"), Content: "second independent work", Async: true})
	if code != 200 || !second.Accepted {
		t.Fatalf("second admission: %d %+v", code, second)
	}
	runForChannel := func(personID, channel string) *control.RunDigest {
		digests, err := store.ListRecentRunsForPerson(ctx, owner.TenantID, personID, 10)
		if err != nil {
			return nil
		}
		for _, digest := range digests {
			if digest.Channel == channel {
				return &digest
			}
		}
		return nil
	}
	var secondID string
	secondParked := func() bool {
		if digest := runForChannel(owner.PersonID, "session-b"); digest != nil {
			secondID = digest.RunID
		}
		if secondID == "" {
			return false
		}
		run, _ := store.GetRun(ctx, owner.TenantID, secondID)
		q, _ := store.GetQueuedByIdempotencyKey(ctx, owner.TenantID, "provider-wait:"+secondID)
		return run != nil && run.Status == "waiting_external" && q != nil
	}
	deadline := time.Now().Add(5 * time.Second)
	for !secondParked() && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if !secondParked() {
		run, _ := store.GetRun(ctx, owner.TenantID, secondID)
		var events []control.Event
		if run != nil {
			events, _ = store.ListTaskEvents(ctx, run.TaskID, 30)
		}
		t.Fatalf("second run did not durably park: run=%+v events=%+v", run, events)
	}
	checkpoint, err := store.IncompleteLoopCheckpointForRun(ctx, owner.TenantID, secondID)
	if err != nil || checkpoint == nil || checkpoint.Detail != "provider_request" {
		t.Fatalf("parked model ledger = %+v %v", checkpoint, err)
	}
	// The first request still owns the physical route. A third person can only
	// reach its own provider wait if the second Agent returned to the pool.
	stranger, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	third, code := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "other",
		Channel: "session-c", Content: "third independent work", Async: true})
	if code != 200 || !third.Accepted {
		t.Fatalf("third admission: %d %+v", code, third)
	}
	waitUntil(t, 5*time.Second, func() bool {
		digest := runForChannel(stranger.PersonID, "session-c")
		return digest != nil && digest.Status == "waiting_external"
	}, "provider wait kept the second Agent occupied")
	provider.releaseNow()
	waitUntil(t, 5*time.Second, func() bool { return daemon.coordinator().activeCount(owner.PersonID) == 0 }, "first run did not settle")
	queue, err := store.GetQueuedByIdempotencyKey(ctx, owner.TenantID, "provider-wait:"+secondID)
	if err != nil || queue == nil {
		t.Fatalf("missing second continuation: %+v %v", queue, err)
	}
	waitUntil(t, 4*time.Second, func() bool { return !time.Now().Before(queue.NotBefore) }, "wait deadline did not arrive")
	daemon.coordinator().drainQueue(owner)
	waitUntil(t, 5*time.Second, func() bool {
		q, _ := store.GetQueuedByIdempotencyKey(ctx, owner.TenantID, "provider-wait:"+secondID)
		return q != nil && q.Status == control.QueueStatusDone && q.RunID != ""
	}, "exact provider continuation did not finish")
}

type onceRateLimitedProvider struct{ calls atomic.Int32 }

func (p *onceRateLimitedProvider) ChatCompletion(context.Context, []llm.Message) (string, error) {
	return "", nil
}
func (p *onceRateLimitedProvider) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Content: "done"}, nil
}
func (p *onceRateLimitedProvider) StreamChat(context.Context, llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	if p.calls.Add(1) == 1 {
		return nil, &llm.ProviderError{Class: llm.ProviderErrorRateLimit, StatusCode: 429, Message: "retry later"}
	}
	out := make(chan llm.StreamEvent, 1)
	out <- llm.StreamEvent{Content: "done"}
	close(out)
	return out, nil
}

func TestRateLimitParksAndResumesFromCheckpoint(t *testing.T) {
	daemon, store, _ := newDetachedRunServer(t, newSlowLLMProvider("unused"))
	provider := &onceRateLimitedProvider{}
	agent := kernel.NewAgent(memory.NewMemoryManager(nil), stubToolBackend{}, llm.NewRequestGate(1).Wrap(provider, "rate-limited"), "test", 1, 3, nil)
	daemon.Gateway = router.NewGateway(agent, nil)
	ctx := context.Background()
	response, code := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local",
		Channel: "session-rate", Content: "complete this work"})
	if code != 200 || response.Outcome == nil || response.Outcome.Status != "waiting_external" ||
		response.Outcome.CompletionReason != "provider_wait" {
		t.Fatalf("rate-limit result = %d %+v", code, response)
	}
	if provider.calls.Load() != 1 {
		t.Fatalf("provider calls before cooldown = %d", provider.calls.Load())
	}
	parent := response.Run
	queue, err := store.GetQueuedByIdempotencyKey(ctx, parent.TenantID, "provider-wait:"+parent.ID)
	if err != nil || queue == nil || queue.NotBefore.Before(time.Now().Add(-time.Second)) {
		t.Fatalf("rate-limit continuation = %+v %v", queue, err)
	}
	checkpoint, err := store.IncompleteLoopCheckpointForRun(ctx, parent.TenantID, parent.ID)
	if err != nil || checkpoint == nil {
		t.Fatalf("missing model-call checkpoint: %+v %v", checkpoint, err)
	}
	userReply, err := store.EnqueueQueued(ctx, control.QueuedTask{TenantID: parent.TenantID,
		PersonID: parent.PersonID, Platform: "cli", PlatformUserID: "local", Channel: "session-rate",
		Content: "also include this requirement", TaskID: parent.TaskID, ReplyToRunID: parent.ID,
		Class: control.QueueClassForeground})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 3*time.Second, func() bool { return !time.Now().Before(queue.NotBefore) }, "rate-limit deadline did not arrive")
	daemon.coordinator().drainQueue(response.Identity)
	waitUntil(t, 5*time.Second, func() bool {
		q, _ := store.GetQueuedByIdempotencyKey(ctx, parent.TenantID, "provider-wait:"+parent.ID)
		return q != nil && q.Status == control.QueueStatusDone && q.RunID != ""
	}, "rate-limited run did not continue")
	waitUntil(t, 5*time.Second, func() bool {
		saved, _ := store.GetQueued(ctx, parent.TenantID, userReply.ID)
		return saved != nil && saved.Status == control.QueueStatusDone
	}, "dependent user input did not continue after provider wait")
	if provider.calls.Load() != 3 {
		t.Fatalf("provider calls = %d; want original, exact child, then user reply", provider.calls.Load())
	}
}
