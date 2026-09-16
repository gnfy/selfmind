package cli

import (
	"context"
	"testing"

	"selfmind/internal/gateway/api"
	"selfmind/internal/modelchange"
	"selfmind/internal/platform/config"
	"selfmind/internal/ui/components"
)

func TestSetupWaitsForAppliedTransactionAndGatewayHealth(t *testing.T) {
	c := NewController("old", "old", nil, "")
	c.SetModelSetup(true)
	m := c.model
	m.modelChangeObserver = func(context.Context, string) (ModelChangeObservation, error) { return ModelChangeObservation{}, nil }
	change := modelchange.Change{ID: "setup-test", Status: modelchange.StatusAwaitingSafeBoundary, Candidate: modelchange.Snapshot{Primary: config.ModelSelectionConfig{Provider: "new", Model: "new"}}}
	_, cmd := m.Update(MsgModelChangeDone{Response: api.ModelChangeResponse{Change: &change, RestartScheduled: true}})
	if cmd == nil || m.modelSetupComplete || m.quitting {
		t.Fatal("restart receipt prematurely completed setup")
	}
	change.Status = modelchange.StatusApplied
	status := modelchange.Status{Running: change.Candidate, Configured: change.Candidate, History: []modelchange.Change{change}, Readiness: modelchange.Readiness{Foreground: true}}
	m.Update(MsgModelChangeObserved{Observation: ModelChangeObservation{Status: status}})
	if m.modelSetupComplete || m.modelChangeID != change.ID {
		t.Fatal("applied state without live health completed setup")
	}
	m.Update(MsgModelChangeObserved{Observation: ModelChangeObservation{Status: status, GatewayReachable: true}})
	if !m.modelSetupComplete {
		t.Fatal("healthy applied transaction did not release onboarding")
	}
}

func TestSetupValidationRequestsAllManagedRoutes(t *testing.T) {
	m := NewController("", "", nil, "").model
	m.modelChangeProcessor = func(_ context.Context, req api.ModelChangeRequest) (api.ModelChangeResponse, error) {
		if req.Action != "validate" || len(req.ValidateRoutes) != 8 {
			t.Fatalf("incomplete setup probe request: %+v", req)
		}
		return api.ModelChangeResponse{}, nil
	}
	cmd := m.validateModelManager(components.SetupValidationRoute, nil, nil, "")
	cmd()
}
