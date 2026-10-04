package llm

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type gateTestProvider struct {
	chat   func(context.Context) (*ChatResponse, error)
	stream func(context.Context) (<-chan StreamEvent, error)
}

func (p gateTestProvider) ChatCompletion(ctx context.Context, _ []Message) (string, error) {
	response, err := p.Chat(ctx, ChatRequest{})
	if response == nil {
		return "", err
	}
	return response.Content, err
}
func (p gateTestProvider) Chat(ctx context.Context, _ ChatRequest) (*ChatResponse, error) {
	return p.chat(ctx)
}
func (p gateTestProvider) StreamChat(ctx context.Context, _ ChatRequest) (<-chan StreamEvent, error) {
	return p.stream(ctx)
}

func TestRequestGateSerializesSameRouteButNotDifferentRoutes(t *testing.T) {
	gate := NewRequestGate(1)
	entered := make(chan struct{}, 3)
	release := make(chan struct{})
	provider := gateTestProvider{chat: func(context.Context) (*ChatResponse, error) {
		entered <- struct{}{}
		<-release
		return &ChatResponse{}, nil
	}}
	first := gate.Wrap(provider, "route-a")
	second := gate.Wrap(provider, "route-a")
	other := gate.Wrap(provider, "route-b")
	done := make(chan error, 3)
	go func() { _, err := first.Chat(context.Background(), ChatRequest{}); done <- err }()
	<-entered
	go func() { _, err := second.Chat(context.Background(), ChatRequest{}); done <- err }()
	go func() { _, err := other.Chat(context.Background(), ChatRequest{}); done <- err }()
	select {
	case <-entered: // different physical route runs while route-a is held
	case <-time.After(time.Second):
		t.Fatal("independent provider route was blocked")
	}
	select {
	case <-entered:
		t.Fatal("same route bypassed request limit")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	for range 3 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("request did not finish")
		}
	}
}

func TestRequestGateSharesRateLimitCooldownAndCancelsWaiter(t *testing.T) {
	gate := NewRequestGate(2)
	var calls atomic.Int32
	provider := gateTestProvider{chat: func(context.Context) (*ChatResponse, error) {
		calls.Add(1)
		return nil, &ProviderError{Class: ProviderErrorRateLimit, StatusCode: 429, Message: "retry-after: 1"}
	}}
	first := gate.Wrap(provider, "route")
	second := gate.Wrap(provider, "route")
	_, err := first.Chat(context.Background(), ChatRequest{})
	if err == nil {
		t.Fatal("expected rate limit")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = second.Chat(ctx, ChatRequest{})
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
		t.Fatalf("cooldown wait = %v, provider calls = %d", err, calls.Load())
	}
}

func TestRequestGateDefersCheckpointedRunWithoutCallingProvider(t *testing.T) {
	gate := NewRequestGate(1)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	provider := gateTestProvider{chat: func(context.Context) (*ChatResponse, error) {
		entered <- struct{}{}
		<-release
		return &ChatResponse{}, nil
	}}
	wrapped := gate.Wrap(provider, "same-physical-route")
	done := make(chan struct{})
	go func() { _, _ = wrapped.Chat(context.Background(), ChatRequest{}); close(done) }()
	<-entered
	ctx := WithProviderWaitDeferral(WithModelContext(context.Background(), ModelContext{RunID: "run-a"}))
	started := time.Now()
	_, err := wrapped.Chat(ctx, ChatRequest{})
	var wait *ProviderWait
	if !errors.As(err, &wait) || wait.Reason != "capacity" || time.Since(started) < providerCapacityGrace || time.Since(started) > time.Second {
		t.Fatalf("capacity deferral = %v after %s", err, time.Since(started))
	}
	close(release)
	<-done
	if _, err := wrapped.Chat(ctx, ChatRequest{}); err != nil {
		t.Fatalf("permit did not reopen: %v", err)
	}
	if got := DeferRateLimit(ctx, &ProviderError{Class: ProviderErrorRateLimit, StatusCode: 429}, time.Second); got == nil || got.Reason != "rate_limit" {
		t.Fatalf("429 was not deferred: %v", got)
	}
	if got := DeferRateLimit(ctx, &ProviderError{Class: ProviderErrorAuth, StatusCode: 401}, time.Second); got != nil {
		t.Fatalf("auth error was deferred: %v", got)
	}
}

func TestRequestGateStreamHoldsPermitUntilCloseAndObservesLate429(t *testing.T) {
	gate := NewRequestGate(1)
	stream := make(chan StreamEvent)
	var calls atomic.Int32
	provider := gateTestProvider{
		chat: func(context.Context) (*ChatResponse, error) {
			calls.Add(1)
			return &ChatResponse{}, nil
		},
		stream: func(context.Context) (<-chan StreamEvent, error) { return stream, nil },
	}
	first := gate.Wrap(provider, "route")
	second := gate.Wrap(provider, "route")
	out, err := first.StreamChat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = second.Chat(ctx, ChatRequest{})
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 0 {
		t.Fatalf("stream permit lost: %v, calls=%d", err, calls.Load())
	}
	stream <- StreamEvent{Err: &ProviderError{Class: ProviderErrorRateLimit, StatusCode: 429}}
	<-out
	close(stream)
	for range out {
	}
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = second.Chat(ctx, ChatRequest{})
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 0 {
		t.Fatalf("late 429 cooldown was lost: %v, calls=%d", err, calls.Load())
	}
}

func TestRequestGateCancellationReleasesStreamPermit(t *testing.T) {
	gate := NewRequestGate(1)
	stream := make(chan StreamEvent)
	provider := gateTestProvider{
		chat:   func(context.Context) (*ChatResponse, error) { return &ChatResponse{}, nil },
		stream: func(context.Context) (<-chan StreamEvent, error) { return stream, nil },
	}
	ctx, cancel := context.WithCancel(context.Background())
	output, err := gate.Wrap(provider, "route").StreamChat(ctx, ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case _, open := <-output:
		if open {
			t.Fatal("canceled stream remained open")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled stream did not terminate")
	}
	callCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if _, err := gate.Wrap(provider, "route").Chat(callCtx, ChatRequest{}); err != nil {
		t.Fatalf("stream cancellation stranded the permit: %v", err)
	}
}

func TestRequestGateAttributesCanceledCapacityWait(t *testing.T) {
	gate := NewRequestGate(1)
	entered := make(chan struct{})
	release := make(chan struct{})
	provider := gateTestProvider{chat: func(context.Context) (*ChatResponse, error) {
		close(entered)
		<-release
		return &ChatResponse{}, nil
	}}
	first := gate.Wrap(provider, "physical-route")
	second := gate.Wrap(provider, "physical-route")
	done := make(chan struct{})
	go func() {
		_, _ = first.Chat(context.Background(), ChatRequest{})
		close(done)
	}()
	<-entered
	var observed struct {
		route, reason, run string
		duration           time.Duration
	}
	gate.SetWaitObserver(func(ctx context.Context, routeID, reason string, duration time.Duration) {
		observed.route, observed.reason, observed.duration = routeID, reason, duration
		observed.run = ModelContextFrom(ctx).RunID
	})
	ctx, cancel := context.WithTimeout(WithModelContext(context.Background(), ModelContext{RunID: "run-2"}), 25*time.Millisecond)
	defer cancel()
	_, err := second.Chat(ctx, ChatRequest{})
	if !errors.Is(err, context.DeadlineExceeded) || observed.route != "physical-route" || observed.reason != "capacity" || observed.run != "run-2" || observed.duration <= 0 {
		t.Fatalf("wait = %v, observation = %+v", err, observed)
	}
	close(release)
	<-done
}
