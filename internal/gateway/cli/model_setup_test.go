package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"selfmind/internal/gateway/api"
	"selfmind/internal/modelchange"
	"selfmind/internal/platform/config"
	"selfmind/internal/ui/components"
)

func TestChatModelManagerCompletesAppliedTransaction(t *testing.T) {
	m := NewController("old", "old", nil, "").model
	m.modelApplying = true
	m.thinking = true
	m.activityText = "Waiting for the configured daemon to become healthy"
	m.modelChangeID = "chat-model-change"
	m.modelManager = components.NewModelManager(components.ModelManagerStatus{}, nil, 100, 30)
	change := modelchange.Change{
		ID: "chat-model-change", Status: modelchange.StatusApplied,
		Candidate: modelchange.Snapshot{Primary: config.ModelSelectionConfig{Provider: "google", Model: "gemini-test"}},
	}
	status := modelchange.Status{
		Running: change.Candidate, Configured: change.Candidate,
		History: []modelchange.Change{change}, Readiness: modelchange.Readiness{Foreground: true},
	}
	m.Update(MsgModelChangeObserved{Observation: ModelChangeObservation{Status: status, GatewayReachable: true}})
	if m.quitting || m.modelApplying || m.thinking || m.modelManager != nil {
		t.Fatalf("chat apply did not return to input: quitting=%v applying=%v thinking=%v manager=%v", m.quitting, m.modelApplying, m.thinking, m.modelManager != nil)
	}
	if m.providerName != "google" || m.modelName != "gemini-test" {
		t.Fatalf("running route = %s/%s", m.providerName, m.modelName)
	}
}

func TestChatModelApplyUsesProgressSurfaceAndOwnsKeys(t *testing.T) {
	m := NewController("old", "old", nil, "").model
	m.width, m.height = 100, 30
	m.modelApplying = true
	m.modelChangePhase = modelchange.StatusRestarting
	m.modelChangePhaseAt = time.Now().Add(-4 * time.Second)
	if cmd := m.startModelOperation("Waiting for the configured daemon to become healthy"); cmd == nil || !m.spinnerRunning {
		t.Fatal("model operation did not start the animation chain")
	}
	view := m.View()
	if !strings.Contains(view, "Waiting for the selected model transaction") || !strings.Contains(view, "restarting (4s)") {
		t.Fatalf("normal chat did not render model progress: %q", view)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if !m.modelApplying {
		t.Fatal("ordinary input escaped the model-transaction state")
	}
}

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

func TestModelManagerWaitsForAppliedTransactionAndGatewayHealth(t *testing.T) {
	c := NewController("old", "old", nil, "")
	c.SetModelManagerOnly(true)
	m := c.model
	m.modelChangeObserver = func(context.Context, string) (ModelChangeObservation, error) { return ModelChangeObservation{}, nil }
	change := modelchange.Change{ID: "manager-test", Status: modelchange.StatusAwaitingSafeBoundary, Candidate: modelchange.Snapshot{Primary: config.ModelSelectionConfig{Provider: "new", Model: "new"}}}
	_, cmd := m.Update(MsgModelChangeDone{Response: api.ModelChangeResponse{Change: &change, RestartScheduled: true}})
	if cmd == nil || m.quitting || !m.modelApplying {
		t.Fatal("model manager treated a restart receipt as successful application")
	}
	change.Status = modelchange.StatusApplied
	status := modelchange.Status{Running: change.Candidate, Configured: change.Candidate, History: []modelchange.Change{change}, Readiness: modelchange.Readiness{Foreground: true}}
	m.Update(MsgModelChangeObserved{Observation: ModelChangeObservation{Status: status}})
	if m.quitting || m.modelChangeID != change.ID {
		t.Fatal("applied state without reachable health closed model manager")
	}
	m.Update(MsgModelChangeObserved{Observation: ModelChangeObservation{Status: status, GatewayReachable: true}})
	if !m.quitting || m.modelApplying {
		t.Fatal("healthy applied transaction did not close model manager")
	}
}

func TestModelManagerReopensRecoveryInsteadOfExiting(t *testing.T) {
	c := NewController("old", "old", nil, "")
	c.SetModelManagerOnly(true)
	m := c.model
	m.modelApplying = true
	m.modelChangeID = "manager-recovery"
	status := modelchange.Status{Pending: &modelchange.Change{
		ID: "manager-recovery", Status: modelchange.StatusRecoveryRequired, Failure: "restart failed",
	}}
	m.Update(MsgModelChangeObserved{Observation: ModelChangeObservation{Status: status, GatewayReachable: true}})
	if m.quitting || m.modelApplying || m.modelManager == nil {
		t.Fatalf("recovery state was not left actionable: quitting=%v applying=%v manager=%v", m.quitting, m.modelApplying, m.modelManager != nil)
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
