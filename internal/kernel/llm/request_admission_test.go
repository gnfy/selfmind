package llm

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestAdmissionDeferralReportsOccupyingRolesAndReleases(t *testing.T) {
	gate := NewRequestGate(1)
	var mu sync.Mutex
	var observations []RequestAdmission
	gate.SetAdmissionObserver(func(_ context.Context, route string, admission RequestAdmission) {
		if route != "opaque-route" {
			t.Errorf("unexpected route: %s", route)
		}
		mu.Lock()
		defer mu.Unlock()
		observations = append(observations, admission)
	})
	entered, release := make(chan struct{}), make(chan struct{})
	provider := gate.Wrap(gateTestProvider{chat: func(context.Context) (*ChatResponse, error) {
		close(entered)
		<-release
		return &ChatResponse{}, nil
	}}, "opaque-route")
	background := WithModelContext(context.Background(), ModelContext{Role: RoleMemoryExtract})
	done := make(chan struct{})
	go func() { _, _ = provider.Chat(background, ChatRequest{}); close(done) }()
	<-entered
	foreground := WithProviderWaitDeferral(WithModelContext(context.Background(), ModelContext{Role: RoleCodingAgent, RunID: "run-current"}))
	_, err := provider.Chat(foreground, ChatRequest{})
	var wait *ProviderWait
	if !errors.As(err, &wait) || wait.Dispatched {
		t.Fatalf("local deferral claimed provider dispatch: %v", err)
	}
	close(release)
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(observations) != 3 || observations[1].Kind != "deferred" || observations[1].Reason != "capacity" ||
		observations[1].Active != 1 || observations[1].ActiveRoles[string(RoleMemoryExtract)] != 1 ||
		observations[2].Kind != "released" || observations[2].Active != 0 || len(observations[2].ActiveRoles) != 0 {
		t.Fatalf("inaccurate capacity evidence: %+v", observations)
	}
}
