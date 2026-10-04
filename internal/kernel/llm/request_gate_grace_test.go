package llm

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestRequestGateBriefCapacityWaitKeepsSameRun(t *testing.T) {
	gate := NewRequestGate(1)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	provider := gateTestProvider{chat: func(context.Context) (*ChatResponse, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return &ChatResponse{}, nil
	}}
	wrapped := gate.Wrap(provider, "physical-route")
	done := make(chan struct{})
	go func() { _, _ = wrapped.Chat(context.Background(), ChatRequest{}); close(done) }()
	<-entered
	timer := time.AfterFunc(30*time.Millisecond, func() { close(release) })
	defer timer.Stop()
	ctx := WithProviderWaitDeferral(WithModelContext(context.Background(), ModelContext{RunID: "same-run"}))
	_, err := wrapped.Chat(ctx, ChatRequest{})
	<-done
	if err != nil || calls.Load() != 2 {
		t.Fatalf("brief contention split the Run: calls=%d err=%v", calls.Load(), err)
	}
}

func TestRequestGateGraceHonorsCancellationAndCooldown(t *testing.T) {
	gate := NewRequestGate(1)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	provider := gateTestProvider{chat: func(context.Context) (*ChatResponse, error) {
		calls.Add(1)
		close(entered)
		<-release
		return &ChatResponse{}, nil
	}}
	wrapped := gate.Wrap(provider, "occupied-route")
	done := make(chan struct{})
	go func() { _, _ = wrapped.Chat(context.Background(), ChatRequest{}); close(done) }()
	<-entered
	ctx, cancel := context.WithTimeout(WithProviderWaitDeferral(WithModelContext(context.Background(), ModelContext{RunID: "checkpointed"})), 20*time.Millisecond)
	_, err := wrapped.Chat(ctx, ChatRequest{})
	cancel()
	close(release)
	<-done
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
		t.Fatalf("canceled grace dispatched request: %v calls=%d", err, calls.Load())
	}
	var rateCalls atomic.Int32
	rate := gate.Wrap(gateTestProvider{chat: func(context.Context) (*ChatResponse, error) {
		rateCalls.Add(1)
		return nil, &ProviderError{Class: ProviderErrorRateLimit, StatusCode: 429}
	}}, "cooling-route")
	_, _ = rate.Chat(context.Background(), ChatRequest{})
	started := time.Now()
	_, err = rate.Chat(WithProviderWaitDeferral(WithModelContext(context.Background(), ModelContext{RunID: "checkpointed"})), ChatRequest{})
	var wait *ProviderWait
	if !errors.As(err, &wait) || wait.Reason != "rate_limit" || time.Since(started) > 100*time.Millisecond || rateCalls.Load() != 1 {
		t.Fatalf("cooldown consumed capacity grace: %v calls=%d", err, rateCalls.Load())
	}
}
