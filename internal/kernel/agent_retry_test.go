package kernel

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

// scriptedStreamProvider returns errFor the first `failN` StreamChat/Chat calls
// then succeeds. It counts calls so tests can assert retry behavior.
type scriptedStreamProvider struct {
	failN       int
	errFor      error
	streamCalls atomic.Int32
	chatCalls   atomic.Int32
}

func (p *scriptedStreamProvider) ChatCompletion(ctx context.Context, messages []llm.Message) (string, error) {
	return "ok", nil
}

func (p *scriptedStreamProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	n := int(p.chatCalls.Add(1))
	if n <= p.failN {
		return nil, p.errFor
	}
	return &llm.ChatResponse{Content: "ok"}, nil
}

func (p *scriptedStreamProvider) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	n := int(p.streamCalls.Add(1))
	if n <= p.failN {
		return nil, p.errFor
	}
	ch := make(chan llm.StreamEvent, 1)
	ch <- llm.StreamEvent{Content: "ok"}
	close(ch)
	return ch, nil
}

func newRetryTestAgent(p llm.Provider) *Agent {
	mem := memory.NewMemoryManager(&mockStorage{})
	return NewAgent(mem, &mockBackend{}, p, "helpful", 1, 5, nil)
}

func TestStreamRetrySucceedsAfterTransientEOF(t *testing.T) {
	p := &scriptedStreamProvider{failN: 2, errFor: io.ErrUnexpectedEOF}
	agent := newRetryTestAgent(p)
	agent.SetRetryPolicy(5, 5*time.Millisecond, 50*time.Millisecond)

	start := time.Now()
	ch, err := agent.streamChatWithRetry(context.Background(), []llm.Message{{Role: "user", Content: "hi"}}, DefaultTaskStrategy())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("expected success after transient EOF, got %v", err)
	}
	if ch == nil {
		t.Fatal("expected a stream channel")
	}
	if got := p.streamCalls.Load(); got != 3 {
		t.Fatalf("expected 3 stream attempts (2 fail + 1 success), got %d", got)
	}
	// Two backoffs must have elapsed (base 5ms + 10ms, with >=0.9 jitter).
	if elapsed < 5*time.Millisecond {
		t.Fatalf("expected backoff sleeps between attempts, elapsed only %v", elapsed)
	}
}

func TestChatRetrySucceedsAfterTransientEOF(t *testing.T) {
	p := &scriptedStreamProvider{failN: 2, errFor: io.EOF}
	agent := newRetryTestAgent(p)
	agent.SetRetryPolicy(5, 2*time.Millisecond, 20*time.Millisecond)

	resp, err := agent.chatResponseWithRetry(context.Background(), []llm.Message{{Role: "user", Content: "hi"}}, DefaultTaskStrategy())
	if err != nil {
		t.Fatalf("expected success after transient EOF, got %v", err)
	}
	if resp == nil || resp.Content != "ok" {
		t.Fatalf("expected ok response, got %+v", resp)
	}
	if got := p.chatCalls.Load(); got != 3 {
		t.Fatalf("expected 3 chat attempts, got %d", got)
	}
}

func TestFatalErrorIsNotRetried(t *testing.T) {
	fatal := errors.New("responses API error 400: context length exceeded")
	p := &scriptedStreamProvider{failN: 100, errFor: fatal}
	agent := newRetryTestAgent(p)
	agent.SetRetryPolicy(5, time.Millisecond, 10*time.Millisecond)

	_, err := agent.streamChatWithRetry(context.Background(), []llm.Message{{Role: "user", Content: "hi"}}, DefaultTaskStrategy())
	if err == nil {
		t.Fatal("expected fatal error to be returned")
	}
	if got := p.streamCalls.Load(); got != 1 {
		t.Fatalf("fatal error must not be retried: got %d attempts, want 1", got)
	}

	_, err = agent.chatResponseWithRetry(context.Background(), []llm.Message{{Role: "user", Content: "hi"}}, DefaultTaskStrategy())
	if err == nil {
		t.Fatal("expected fatal error to be returned from chat")
	}
	if got := p.chatCalls.Load(); got != 1 {
		t.Fatalf("fatal error must not be retried in chat: got %d attempts, want 1", got)
	}
}

func TestRetryBackoffIsContextCancellable(t *testing.T) {
	// Always-EOF provider with a very large backoff base: a cancelled context
	// must abort the pending backoff sleep promptly rather than waiting it out.
	p := &scriptedStreamProvider{failN: 100, errFor: io.ErrUnexpectedEOF}
	agent := newRetryTestAgent(p)
	agent.SetRetryPolicy(5, 10*time.Second, 30*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := agent.streamChatWithRetry(ctx, []llm.Message{{Role: "user", Content: "hi"}}, DefaultTaskStrategy())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("cancellation should interrupt the 10s backoff quickly, took %v", elapsed)
	}
}
