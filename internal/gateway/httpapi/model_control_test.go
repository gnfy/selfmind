package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/modelchange"
	"selfmind/internal/modelruntime"
	"selfmind/internal/platform/config"
)

func TestModelControlRejectsLegacyTextSubcommands(t *testing.T) {
	service, _ := testModelChangeService(t)
	server := &Server{ModelChanges: service}
	for _, command := range []string{"/model status", "/model set background deepseek deepseek-chat", "/model confirm model_123", "/model history"} {
		reply, err := server.handleModelControl(context.Background(), "weixin", command)
		if err != nil {
			t.Fatal(err)
		}
		if reply != "Usage: /model" {
			t.Fatalf("%q reply = %q", command, reply)
		}
	}
}

func TestGatewayModelChangeEndpointReturnsStructuredPreview(t *testing.T) {
	service, _ := testModelChangeService(t)
	server := &Server{ModelChanges: service, LocalControlToken: "local-secret"}
	reasoning := "high"
	body, _ := json.Marshal(api.ModelChangeRequest{
		Action: "prepare", Route: "primary", Provider: "codex-cli", Model: "gpt-5.6-sol", Reasoning: &reasoning,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/gateway/model/change", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:43210"
	req.Header.Set(api.LocalControlTokenHeader, "local-secret")
	recorder := httptest.NewRecorder()
	server.handleGatewayModelChange(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response api.ModelChangeResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Change == nil || !response.NeedsConfirm || response.Change.Candidate.Primary.Model != "gpt-5.6-sol" {
		t.Fatalf("response = %+v", response)
	}
}

func TestGatewayModelCancelResumesWorkParkedByReadiness(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	service, _ := testModelChangeService(t)
	if _, err := service.AcceptMigrationReadiness(); err != nil {
		t.Fatal(err)
	}
	status, err := service.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	candidate := status.Running
	candidate.Primary.Model = "gpt-preview"
	prepared, err := service.Prepare(context.Background(), modelchange.PrepareRequest{
		Candidate: candidate, Source: "test", RequireConfirmation: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	daemon.ModelChanges = service
	daemon.LocalControlToken = "local-secret"
	response, statusCode := daemon.ProcessMessage(context.Background(), api.MessageRequest{
		TenantID: identity.TenantID, Platform: "cli", PlatformUserID: identity.PlatformUserID,
		Channel: "cli", Content: "run after the preview is cancelled", Async: true,
	})
	if statusCode != http.StatusOK || !response.Accepted || response.Turn == nil || response.Turn.Status != "queued" {
		t.Fatalf("parked response = status:%d body:%+v", statusCode, response)
	}
	body, _ := json.Marshal(api.ModelChangeRequest{Action: "cancel", ChangeID: prepared.Change.ID})
	req := httptest.NewRequest(http.MethodPost, "/v1/gateway/model/change", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:43210"
	req.Header.Set(api.LocalControlTokenHeader, "local-secret")
	recorder := httptest.NewRecorder()
	daemon.handleGatewayModelChange(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("cancel status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		count, countErr := store.CountQueued(context.Background(), identity.TenantID, identity.PersonID, control.QueueStatusQueued)
		if countErr == nil && count == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	count, countErr := store.CountQueued(context.Background(), identity.TenantID, identity.PersonID, control.QueueStatusQueued)
	after, inspectErr := service.Inspect()
	t.Fatalf("queued work did not resume after cancel: count=%d err=%v ready=%v inspect_err=%v pending=%+v running_verified=%v configured=%+v running=%+v", count, countErr, after.ModelReady(), inspectErr, after.Pending, after.RunningVerifiedAt, after.Configured, after.Running)
}

func TestGatewayModelChangeValidatesWholeDraftWithoutWriting(t *testing.T) {
	service, path := testModelChangeService(t)
	server := &Server{ModelChanges: service, LocalControlToken: "local-secret"}
	reasoning := "high"
	body, _ := json.Marshal(api.ModelChangeRequest{
		Action: "validate",
		Patches: []api.ModelSelectionPatch{
			{Route: "primary", Provider: "codex-cli", Model: "gpt-5.6-sol", Reasoning: &reasoning},
			{Route: "background", Provider: "deepseek", Model: "deepseek-chat"},
			{Route: "memory_extract", Provider: "anthropic", Model: "claude-role"},
		},
		ValidateRoutes: []string{"memory_extract"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/gateway/model/change", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:43210"
	req.Header.Set(api.LocalControlTokenHeader, "local-secret")
	recorder := httptest.NewRecorder()
	server.handleGatewayModelChange(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response api.ModelChangeResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Probes) != 1 || response.Probes[0].Route != modelchange.RouteMemoryExtract || !response.Probes[0].OK {
		t.Fatalf("response = %+v", response)
	}
	status, err := service.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if status.Pending != nil || status.Configured.Primary.Model != "gpt-5.5" {
		t.Fatalf("validation mutated state: %+v", status)
	}
	cfg, err := config.LoadConfig(config.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := cfg.Models.Roles["memory_extract"]; exists {
		t.Fatalf("validation wrote role config: %+v", cfg.Models.Roles)
	}
}

func TestGatewayModelValidationSavesNewProviderCredentialOnlyAfterSuccessfulProbe(t *testing.T) {
	service, path := testModelChangeService(t)
	service.Validate = func(_ context.Context, cfg *config.Config, routes []modelchange.Route) []modelchange.ProbeResult {
		if cfg.ProviderProfiles["deepseek"].APIKey != "sk-deepseek" {
			return []modelchange.ProbeResult{{Route: routes[0], Error: "credential was not available to probe"}}
		}
		return []modelchange.ProbeResult{{Route: routes[0], OK: true, Provider: "deepseek", Model: "deepseek-chat"}}
	}
	server := &Server{ModelChanges: service, LocalControlToken: "local-secret"}
	body, _ := json.Marshal(api.ModelChangeRequest{
		Action: "validate",
		Patches: []api.ModelSelectionPatch{{
			Route: "background", Provider: "deepseek", Model: "deepseek-chat", APIKey: "sk-deepseek",
		}},
		ValidateRoutes: []string{"background"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/gateway/model/change", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:43210"
	req.Header.Set(api.LocalControlTokenHeader, "local-secret")
	recorder := httptest.NewRecorder()
	server.handleGatewayModelChange(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "sk-deepseek") {
		t.Fatal("response leaked credential")
	}
	cfg, err := config.LoadConfig(config.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if got := modelruntime.NewCredentialStore(cfg.Auth.CredentialsFile).Resolve("deepseek").Token; got != "sk-deepseek" {
		t.Fatalf("stored token = %q", got)
	}
}

func TestGatewayModelValidationDoesNotSaveCredentialAfterFailedProbe(t *testing.T) {
	service, path := testModelChangeService(t)
	service.Validate = func(_ context.Context, cfg *config.Config, routes []modelchange.Route) []modelchange.ProbeResult {
		if cfg.ProviderProfiles["deepseek"].APIKey != "sk-rejected" {
			t.Fatal("credential was not available to transient probe config")
		}
		return []modelchange.ProbeResult{{Route: routes[0], Error: "authentication failed"}}
	}
	server := &Server{ModelChanges: service, LocalControlToken: "local-secret"}
	body, _ := json.Marshal(api.ModelChangeRequest{
		Action: "validate",
		Patches: []api.ModelSelectionPatch{{
			Route: "background", Provider: "deepseek", Model: "deepseek-chat", APIKey: "sk-rejected",
		}},
		ValidateRoutes: []string{"background"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/gateway/model/change", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:43210"
	req.Header.Set(api.LocalControlTokenHeader, "local-secret")
	recorder := httptest.NewRecorder()
	server.handleGatewayModelChange(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "sk-rejected") {
		t.Fatal("response leaked credential")
	}
	cfg, err := config.LoadConfig(config.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if got := modelruntime.NewCredentialStore(cfg.Auth.CredentialsFile).Resolve("deepseek").Token; got != "" {
		t.Fatalf("failed credential was stored: %q", got)
	}
}

func TestGatewayModelChangeAppliesPrimaryBackgroundAndRolesInOneTransaction(t *testing.T) {
	service, _ := testModelChangeService(t)
	server := &Server{
		ModelChanges: service, LocalControlToken: "local-secret",
		ModelRestartFunc: func(string) error { return nil },
	}
	body, _ := json.Marshal(api.ModelChangeRequest{
		Action: "apply",
		Patches: []api.ModelSelectionPatch{
			{Route: "primary", Provider: "codex-cli", Model: "gpt-5.6-sol"},
			{Route: "background", Provider: "deepseek", Model: "deepseek-chat"},
			{Route: "memory_extract", Provider: "anthropic", Model: "claude-role"},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/gateway/model/change", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:43210"
	req.Header.Set(api.LocalControlTokenHeader, "local-secret")
	recorder := httptest.NewRecorder()
	server.handleGatewayModelChange(recorder, req)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response api.ModelChangeResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Change == nil || len(response.Change.ChangedRoutes) != 3 {
		t.Fatalf("response = %+v", response)
	}
	if got := modelchange.SelectionForRoute(response.Change.Candidate, modelchange.RouteMemoryExtract); got.Provider != "anthropic" || got.Model != "claude-role" {
		t.Fatalf("role = %+v", got)
	}
}

func TestGatewayModelChangeApplyReportsRestartScheduling(t *testing.T) {
	service, _ := testModelChangeService(t)
	called := ""
	server := &Server{
		ModelChanges: service, LocalControlToken: "local-secret",
		ModelRestartFunc: func(changeID string) error { called = changeID; return nil },
	}
	body, _ := json.Marshal(api.ModelChangeRequest{
		Action: "apply", Route: "background", Provider: "codex-cli", Model: "gpt-5.6-sol",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/gateway/model/change", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:43210"
	req.Header.Set(api.LocalControlTokenHeader, "local-secret")
	recorder := httptest.NewRecorder()
	server.handleGatewayModelChange(recorder, req)
	var response api.ModelChangeResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusAccepted || response.Change == nil || !response.RestartScheduled || called != response.Change.ID {
		t.Fatalf("status=%d response=%+v called=%q", recorder.Code, response, called)
	}
}

func TestGatewayModelChangeConfirmRetryDoesNotSpawnSecondRestart(t *testing.T) {
	service, _ := testModelChangeService(t)
	calls := 0
	server := &Server{ModelChanges: service, LocalControlToken: "local-secret", ModelRestartFunc: func(string) error { calls++; return nil }}
	prepared, err := service.Prepare(context.Background(), modelchange.PrepareRequest{
		Candidate: func() modelchange.Snapshot {
			status, _ := service.Inspect()
			candidate := status.Running
			candidate.Primary.Model = "gpt-idempotent"
			return candidate
		}(),
		Source: "http", RequireConfirmation: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		body, _ := json.Marshal(api.ModelChangeRequest{Action: "confirm", ChangeID: prepared.Change.ID})
		req := httptest.NewRequest(http.MethodPost, "/v1/gateway/model/change", bytes.NewReader(body))
		req.RemoteAddr = "127.0.0.1:43210"
		req.Header.Set(api.LocalControlTokenHeader, "local-secret")
		recorder := httptest.NewRecorder()
		server.handleGatewayModelChange(recorder, req)
		if recorder.Code != http.StatusAccepted {
			t.Fatalf("attempt %d status=%d body=%s", i, recorder.Code, recorder.Body.String())
		}
	}
	if calls != 1 {
		t.Fatalf("restart helper calls = %d, want 1", calls)
	}
}

func TestModelControlStatusDistinguishesRunningConfiguredPending(t *testing.T) {
	service, _ := testModelChangeService(t)
	server := &Server{ModelChanges: service}
	current, err := service.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	candidate := current.Configured
	candidate.Primary.Model = "gpt-5.6-sol"
	if _, err := service.Prepare(context.Background(), modelchange.PrepareRequest{Candidate: candidate, Source: "test", RequireConfirmation: true}); err != nil {
		t.Fatal(err)
	}
	status, err := server.handleModelControl(context.Background(), "cli", "/model")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Running primary", "Pending: model_", "memory_extract: uses background model", "Generation:"} {
		if !strings.Contains(status, want) {
			t.Fatalf("status missing %q: %s", want, status)
		}
	}
}

func testModelChangeService(t *testing.T) (*modelchange.Service, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.LoadConfig(config.Options{Path: path, CreateIfMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	cfg.SetPrimaryModel("codex-cli", "gpt-5.5", "medium")
	cfg.Auth.CredentialsFile = filepath.Join(filepath.Dir(path), "auth.json")
	cfg.Models.Primary.ServiceTier = "priority"
	cfg.Models.Primary.ContextLength = 123456
	cfg.Models.Auxiliary = config.ModelSelectionConfig{
		Provider: "codex-cli", Model: "gpt-background", Reasoning: "low",
		ServiceTier: "standard", ContextLength: 654321,
	}
	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	service := &modelchange.Service{
		ConfigPath: path,
		Validate: func(_ context.Context, cfg *config.Config, routes []modelchange.Route) []modelchange.ProbeResult {
			results := make([]modelchange.ProbeResult, 0, len(routes))
			for _, route := range routes {
				selection := modelchange.SelectionForRoute(modelchange.SnapshotFromConfig(cfg), route)
				results = append(results, modelchange.ProbeResult{
					Route: route, OK: true, Provider: selection.Provider, Model: selection.Model,
				})
			}
			return results
		},
	}
	return service, path
}
