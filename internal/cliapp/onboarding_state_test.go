package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfmind/internal/buildinfo"
	"selfmind/internal/gateway/api"
	"selfmind/internal/modelchange"
	"selfmind/internal/platform/config"
)

func TestOnboardingResumesRuntimeWithoutRepeatingReadyModels(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	cfg := &config.Config{
		Path: configPath,
		Models: config.ModelsConfig{
			Primary:   config.ModelSelectionConfig{Provider: "codex-cli", Model: "gpt-main"},
			Auxiliary: config.ModelSelectionConfig{Provider: "deepseek", Model: "deepseek-background"},
		},
	}
	cfg.Normalize()
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	models := &modelchange.Service{ConfigPath: configPath}
	if _, err := models.AcceptMigrationReadiness(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	app := &App{
		ctx: context.Background(), stdout: &stdout, stderr: &stderr, configPath: configPath,
	}
	got, code := app.ensureOnboarding(cfg, onboardingOptions{NonInteractive: true, SkipGateway: true})
	if code != 0 || got == nil {
		t.Fatalf("onboarding cfg=%v code=%d stderr=%q", got != nil, code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Models ready") || strings.Contains(stdout.String(), "Opening Model Manager") {
		t.Fatalf("resume output did not preserve completed model stage: %q", stdout.String())
	}
}

func TestReadyModelsProceedToManagedRuntimeRepairWithoutModelPromptOrProbe(t *testing.T) {
	workspace := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{Path: configPath}
	cfg.SetPrimaryModel("codex-cli", "gpt-main", "")
	cfg.Models.Auxiliary = config.ModelSelectionConfig{Provider: "deepseek", Model: "deepseek-background"}
	cfg.Normalize()
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	models := &modelchange.Service{
		ConfigPath: configPath,
		Validate: func(_ context.Context, cfg *config.Config, routes []modelchange.Route) []modelchange.ProbeResult {
			results := make([]modelchange.ProbeResult, 0, len(routes))
			for _, route := range routes {
				results = append(results, modelchange.ProbeResult{Route: route, OK: true})
			}
			return results
		},
	}
	status, rolledBack, err := models.ReconcileStartup(context.Background())
	if err != nil || rolledBack || status.Pending == nil {
		t.Fatalf("apply initial model transaction: status=%+v rolledBack=%v err=%v", status, rolledBack, err)
	}
	status, err = models.MarkStartupHealthy()
	if err != nil || !status.ModelReady() || len(status.History) != 1 || status.History[0].Status != modelchange.StatusApplied {
		t.Fatalf("applied model transaction = %+v, err=%v", status, err)
	}
	initialState := onboardingState{Version: onboardingStateVersion, WorkspacePath: workspace, ApprovalMode: "smart", BackgroundMode: "managed"}
	if err := saveOnboardingState(onboardingStatePath(cfg, configPath), initialState); err != nil {
		t.Fatal(err)
	}
	var actions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/workspaces/register":
			actions = append(actions, "register workspace")
			_ = json.NewEncoder(w).Encode(map[string]any{"workspace": map[string]any{
				"id": "ws-test", "name": "project", "local_path": workspace,
			}})
		case "/v1/message":
			actions = append(actions, "set safety")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("SELF_GATEWAY_URL", server.URL)
	cfg.Gateway.URL = server.URL
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	app := &App{
		ctx: context.Background(), stdout: &stdout, stderr: &stderr, configPath: configPath,
		managedGatewayReconcile: func() (gatewayServiceInstallReceipt, error) {
			actions = append(actions, "reconcile managed service")
			return gatewayServiceInstallReceipt{Manager: "launchd", Generation: "service-test"}, nil
		},
		managedServiceHealthy: func() bool { return true },
		onboardingGatewayStatus: func(context.Context) (api.GatewayStatusResponse, error) {
			resolvedConfig, _ := config.ResolveConfigPath(configPath)
			return api.GatewayStatusResponse{
				Runtime: api.GatewayRuntimeInfo{
					Version: buildinfo.Version, ConfigPath: resolvedConfig,
					ServiceManager: "launchd", ServiceGeneration: "service-test",
					ModelRouteFingerprint: modelchange.SnapshotFromConfig(cfg).Fingerprint(),
				},
				State: "running", StoreSchema: api.StoreSchemaHealth{Version: 1, CurrentVersion: 1},
			}, nil
		},
	}
	got, code := app.ensureOnboarding(cfg, onboardingOptions{NonInteractive: true})
	if code != 0 || got == nil {
		t.Fatalf("runtime repair code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	want := []string{"reconcile managed service", "register workspace", "set safety"}
	if strings.Join(actions, "|") != strings.Join(want, "|") {
		t.Fatalf("runtime repair actions = %v, want %v", actions, want)
	}
	if strings.Contains(stdout.String(), "Continue with these models?") || strings.Contains(stdout.String(), "Verifying models") {
		t.Fatalf("runtime repair repeated model setup: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Setup ready.") || strings.Contains(stdout.String(), "Setup is incomplete") {
		t.Fatalf("managed runtime readiness was not accepted: %q", stdout.String())
	}
	state, err := loadOnboardingState(onboardingStatePath(cfg, configPath))
	if err != nil {
		t.Fatal(err)
	}
	if state.ServiceGeneration != "service-test" || !state.runtimeReady() {
		t.Fatalf("runtime repair receipt = %+v", state)
	}
	if !app.expectedBackgroundStateReady(state) {
		t.Fatal("managed runtime ownership was not verified by the coordinator")
	}
}

func TestManagedGatewayReadinessRequiresServiceOwnership(t *testing.T) {
	state := onboardingState{
		BackgroundMode: "managed", BackgroundManager: "launchd", ServiceGeneration: "service-current",
	}
	runtime := api.GatewayRuntimeInfo{
		ServiceManager: "launchd", ServiceGeneration: "service-current",
		Version: "v-test", ConfigPath: "/work/config.yaml",
	}
	if !managedGatewayOwned(state, true, runtime, "v-test", "/work/config.yaml") {
		t.Fatal("matching running service and Gateway identity should be owned")
	}
	for name, mutate := range map[string]func(*onboardingState, *api.GatewayRuntimeInfo, *bool){
		"job not running": func(_ *onboardingState, _ *api.GatewayRuntimeInfo, healthy *bool) { *healthy = false },
		"unowned gateway": func(_ *onboardingState, runtime *api.GatewayRuntimeInfo, _ *bool) { runtime.ServiceManager = "" },
		"stale generation": func(_ *onboardingState, runtime *api.GatewayRuntimeInfo, _ *bool) {
			runtime.ServiceGeneration = "service-old"
		},
		"wrong version": func(_ *onboardingState, runtime *api.GatewayRuntimeInfo, _ *bool) { runtime.Version = "v-old" },
		"wrong config": func(_ *onboardingState, runtime *api.GatewayRuntimeInfo, _ *bool) {
			runtime.ConfigPath = "/other/config.yaml"
		},
		"missing config": func(_ *onboardingState, runtime *api.GatewayRuntimeInfo, _ *bool) {
			runtime.ConfigPath = ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidateState := state
			candidateRuntime := runtime
			healthy := true
			mutate(&candidateState, &candidateRuntime, &healthy)
			if managedGatewayOwned(candidateState, healthy, candidateRuntime, "v-test", "/work/config.yaml") {
				t.Fatal("mismatched managed Gateway was reported ready")
			}
		})
	}
}

func TestPersistManagedGatewayReceiptCommitsReplacementGeneration(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	app := &App{configPath: configPath}
	if err := app.persistManagedGatewayReceipt(gatewayServiceInstallReceipt{
		Manager: "launchd", Generation: "service-new",
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(config.Options{Path: configPath})
	if err != nil {
		t.Fatal(err)
	}
	state, err := loadOnboardingState(onboardingStatePath(cfg, configPath))
	if err != nil {
		t.Fatal(err)
	}
	if state.BackgroundMode != "managed" || state.BackgroundManager != "launchd" ||
		state.ServiceGeneration != "service-new" || state.GatewayVerifiedAt.IsZero() {
		t.Fatalf("persisted receipt = %+v", state)
	}
}

func TestManagedBackgroundStatusDistinguishesOwnershipStates(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	cfg := &config.Config{Path: configPath}
	cfg.Normalize()
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	fingerprint := modelchange.SnapshotFromConfig(cfg).Fingerprint()
	compatible := api.GatewayStatusResponse{
		Runtime: api.GatewayRuntimeInfo{
			Version: buildinfo.Version, ConfigPath: configPath, ModelRouteFingerprint: fingerprint,
		},
		State: "running", StoreSchema: api.StoreSchemaHealth{Version: 1, CurrentVersion: 1},
	}
	owned := compatible
	owned.Runtime.ServiceManager = "launchd"
	owned.Runtime.ServiceGeneration = "service-current"
	receipt := onboardingState{
		BackgroundMode: "managed", BackgroundManager: "launchd", ServiceGeneration: "service-current",
	}
	conflicting := compatible
	conflicting.Runtime.ModelRouteFingerprint = "stale-route"

	for name, test := range map[string]struct {
		installed bool
		running   bool
		status    *api.GatewayStatusResponse
		want      managedBackgroundStatus
	}{
		"absent":      {want: managedBackgroundAbsent},
		"starting":    {installed: true, running: true, want: managedBackgroundStarting},
		"unhealthy":   {installed: true, want: managedBackgroundUnhealthy},
		"healthy":     {installed: true, running: true, status: &owned, want: managedBackgroundHealthy},
		"degraded":    {installed: true, status: &compatible, want: managedBackgroundDegraded},
		"conflicting": {installed: true, running: true, status: &conflicting, want: managedBackgroundConflicting},
	} {
		t.Run(name, func(t *testing.T) {
			got := classifyManagedBackground(test.installed, test.running, receipt, test.status, buildinfo.Version, configPath)
			if got != test.want {
				t.Fatalf("status = %q, want %q", got, test.want)
			}
		})
	}
	onDemand := receipt
	onDemand.BackgroundMode = "on-demand"
	if got := classifyManagedBackground(false, false, onDemand, &compatible, buildinfo.Version, configPath); got != managedBackgroundAbsent {
		t.Fatalf("on-demand compatible Gateway status = %q, want %q", got, managedBackgroundAbsent)
	}
}

func TestActiveGatewayKeepsForegroundAvailableAsRuntimeDegraded(t *testing.T) {
	t.Setenv("SELF_GATEWAY_DRAIN_TIMEOUT", "20ms")
	workspace := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{Path: configPath}
	cfg.Normalize()
	modelFingerprint := modelchange.SnapshotFromConfig(cfg).Fingerprint()
	resolvedConfig, _ := config.ResolveConfigPath(configPath)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/gateway/shutdown":
			w.WriteHeader(http.StatusAccepted)
		case "/v1/gateway/status":
			_ = json.NewEncoder(w).Encode(api.GatewayStatusResponse{
				Runtime: api.GatewayRuntimeInfo{
					Version: buildinfo.Version, ConfigPath: resolvedConfig, ModelRouteFingerprint: modelFingerprint,
				},
				State: "running", ActiveRunCount: 1,
				StoreSchema: api.StoreSchemaHealth{Version: 1, CurrentVersion: 1},
			})
		case "/v1/workspaces/register":
			_ = json.NewEncoder(w).Encode(map[string]any{"workspace": map[string]any{
				"id": "ws-test", "name": "project", "local_path": workspace,
			}})
		case "/v1/message":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("SELF_GATEWAY_URL", server.URL)
	cfg.Gateway.URL = server.URL
	cfg.Normalize()
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	state := onboardingState{WorkspacePath: workspace, ApprovalMode: "smart", BackgroundMode: "managed"}
	var stdout, stderr bytes.Buffer
	app := &App{ctx: context.Background(), stdout: &stdout, stderr: &stderr, configPath: configPath}
	if code := app.runOnboardingRuntimeStep(&state, onboardingOptions{NonInteractive: true, SkipModel: true}); code != 0 {
		t.Fatalf("degraded runtime code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "Background service healthy") || !strings.Contains(stdout.String(), "Background startup needs repair") {
		t.Fatalf("degraded runtime output = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "SelfMind is available") {
		t.Fatalf("missing degraded foreground notice: %q", stderr.String())
	}
	if state.BackgroundManager != "" || state.ServiceGeneration != "" || !state.runtimeReady() {
		t.Fatalf("degraded runtime receipt = %+v", state)
	}
}

func TestCompatibleGatewayWithModelDriftCannotBeRuntimeDegraded(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	cfg := &config.Config{Path: configPath}
	cfg.Normalize()
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	status := api.GatewayStatusResponse{
		Runtime: api.GatewayRuntimeInfo{
			Version: buildinfo.Version, ConfigPath: configPath, ModelRouteFingerprint: "stale-route",
		},
		State: "running", StoreSchema: api.StoreSchemaHealth{Version: 1, CurrentVersion: 1},
	}
	if compatibleOnboardingGateway(status, buildinfo.Version, configPath) {
		t.Fatal("Gateway with model-route drift was accepted for Runtime Degraded")
	}
}

func TestOnboardingRuntimeReadinessDoesNotOwnModelRoutes(t *testing.T) {
	now := time.Now().UTC()
	state := onboardingState{
		Version:        onboardingStateVersion,
		BackgroundMode: "on-demand", GatewayVerifiedAt: now,
		WorkspaceID: "ws-1", WorkspacePath: "/work/project", WorkspaceTrusted: true,
		ApprovalMode: "smart",
	}
	if !state.runtimeReady() {
		t.Fatal("complete runtime receipt should be ready")
	}
	state.Primary = onboardingModelState{Provider: "changed", Model: "ignored", VerifiedAt: now}
	if !state.runtimeReady() {
		t.Fatal("legacy model fields must not affect runtime readiness")
	}
}

func TestOnboardingStatePersistsFirstSuccessfulTask(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".selfmind", "onboarding.json")
	state := onboardingState{BackgroundMode: "on-demand"}
	state.recordFirstTask()
	if err := saveOnboardingState(path, state); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadOnboardingState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.FirstTaskCompleted || loaded.FirstTaskCompletedAt.IsZero() || loaded.Version != onboardingStateVersion {
		t.Fatalf("loaded receipt = %+v", loaded)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("receipt mode = %o, want 600", got)
	}
}

func TestOnboardingV2StopsPersistingLegacyModelReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".selfmind", "onboarding.json")
	now := time.Now().UTC()
	state := onboardingState{
		Version:           onboardingStateVersion - 1,
		Primary:           onboardingModelState{Provider: "codex-cli", Model: "gpt-main", VerifiedAt: now},
		Auxiliary:         onboardingModelState{Provider: "deepseek", Model: "deepseek-background", VerifiedAt: now},
		AuxiliaryDegraded: true,
		BackgroundMode:    "on-demand", GatewayVerifiedAt: now,
		WorkspaceID: "ws-1", WorkspacePath: "/work/project", WorkspaceTrusted: true,
		ApprovalMode: "smart",
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	legacyData := append(append([]byte(nil), data...), '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, legacyData, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadOnboardingState(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveOnboardingState(path, loaded); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, legacy := range []string{"\"primary\"", "\"auxiliary\"", "auxiliary_degraded"} {
		if bytes.Contains(data, []byte(legacy)) {
			t.Fatalf("v2 onboarding state retained legacy model authority %q:\n%s", legacy, data)
		}
	}
	backup, err := os.ReadFile(path + ".v1.backup")
	if err != nil {
		t.Fatalf("legacy onboarding backup: %v", err)
	}
	if !bytes.Equal(backup, legacyData) {
		t.Fatalf("legacy onboarding backup changed:\n%s", backup)
	}
}

func TestLegacyOnboardingModelReceiptMigratesWithoutAnotherProbe(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	cfg := &config.Config{
		Path: configPath,
		Models: config.ModelsConfig{
			Primary:   config.ModelSelectionConfig{Provider: "codex-cli", Model: "gpt-main"},
			Auxiliary: config.ModelSelectionConfig{Provider: "deepseek", Model: "deepseek-background"},
		},
	}
	cfg.Normalize()
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	models := &modelchange.Service{ConfigPath: configPath}
	status, err := models.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if status.ModelReady() {
		t.Fatal("fresh model state must not be ready before migration evidence is accepted")
	}

	now := time.Now().UTC()
	legacy := onboardingState{
		Version:   1,
		Primary:   onboardingModelState{Provider: "codex-cli", Model: "gpt-main", VerifiedAt: now},
		Auxiliary: onboardingModelState{Provider: "deepseek", Model: "deepseek-background", VerifiedAt: now},
	}
	data, err := json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	statePath := onboardingStatePath(cfg, configPath)
	if err := os.WriteFile(statePath, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	app := &App{
		ctx: context.Background(), stdout: &stdout, stderr: &stderr, configPath: configPath,
	}
	if _, code := app.ensureOnboarding(cfg, onboardingOptions{NonInteractive: true, SkipGateway: true}); code != 0 {
		t.Fatalf("migration code=%d stderr=%q", code, stderr.String())
	}
	status, err = models.Inspect()
	if err != nil || !status.ModelReady() {
		t.Fatalf("migrated model readiness=%v err=%v", status.ModelReady(), err)
	}
	migrated, err := loadOnboardingState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.Version != onboardingStateVersion || migrated.Primary.Provider != "" || migrated.Auxiliary.Provider != "" {
		t.Fatalf("legacy onboarding authority was not retired: %+v", migrated)
	}
	if _, err := os.Stat(statePath + ".v1.backup"); err != nil {
		t.Fatalf("legacy onboarding backup: %v", err)
	}

	second := &App{
		ctx: context.Background(), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, configPath: configPath,
	}
	if _, code := second.ensureOnboarding(cfg, onboardingOptions{NonInteractive: true, SkipGateway: true}); code != 0 {
		t.Fatalf("idempotent migration code=%d", code)
	}
}

func TestLegacyDegradedBackgroundReceiptDoesNotEstablishModelReadiness(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	cfg := &config.Config{
		Path: configPath,
		Models: config.ModelsConfig{
			Primary:   config.ModelSelectionConfig{Provider: "codex-cli", Model: "gpt-main"},
			Auxiliary: config.ModelSelectionConfig{Provider: "deepseek", Model: "deepseek-background"},
		},
	}
	cfg.Normalize()
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	state := onboardingState{
		Version:           1,
		Primary:           onboardingModelState{Provider: "codex-cli", Model: "gpt-main", VerifiedAt: now},
		Auxiliary:         onboardingModelState{Provider: "deepseek", Model: "deepseek-background"},
		AuxiliaryDegraded: true,
	}
	ready, err := (&App{configPath: configPath}).modelReadiness(cfg, &state)
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		t.Fatal("legacy degraded Background receipt established Model Readiness")
	}
}

func TestFailedRuntimeSetupResumesWithoutRepeatingModelStage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(home)
	configPath := filepath.Join(home, ".selfmind", "config.yaml")
	cfg := &config.Config{
		Path: configPath,
		Models: config.ModelsConfig{
			Primary:   config.ModelSelectionConfig{Provider: "codex-cli", Model: "gpt-main"},
			Auxiliary: config.ModelSelectionConfig{Provider: "deepseek", Model: "deepseek-background"},
		},
	}
	cfg.Normalize()
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := (&modelchange.Service{ConfigPath: configPath}).AcceptMigrationReadiness(); err != nil {
		t.Fatal(err)
	}
	managedReconcile := func() (gatewayServiceInstallReceipt, error) {
		return gatewayServiceInstallReceipt{}, errors.New("test runtime setup failure")
	}

	firstOut, firstErr := &bytes.Buffer{}, &bytes.Buffer{}
	first := &App{
		ctx: context.Background(), stdout: firstOut, stderr: firstErr, configPath: configPath,
		managedGatewayReconcile: managedReconcile,
	}
	if _, code := first.ensureOnboarding(cfg, onboardingOptions{NonInteractive: true}); code == 0 {
		t.Fatalf("runtime setup in the home directory unexpectedly succeeded: %q", firstOut.String())
	}
	secondOut, secondErr := &bytes.Buffer{}, &bytes.Buffer{}
	second := &App{
		ctx: context.Background(), stdout: secondOut, stderr: secondErr, configPath: configPath,
		managedGatewayReconcile: managedReconcile,
	}
	if _, code := second.ensureOnboarding(cfg, onboardingOptions{NonInteractive: true}); code == 0 {
		t.Fatalf("runtime repair in the home directory unexpectedly succeeded: %q", secondOut.String())
	}
	if !strings.Contains(secondOut.String(), "Models ready") || strings.Contains(secondOut.String(), "Model setup") {
		t.Fatalf("resume output = %q", secondOut.String())
	}
}

func TestOnboardingMissingReadinessOpensSoleModelManager(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	cfg := &config.Config{
		Path: configPath,
		Models: config.ModelsConfig{
			Primary:   config.ModelSelectionConfig{Provider: "codex-cli", Model: "gpt-main"},
			Auxiliary: config.ModelSelectionConfig{Provider: "deepseek", Model: "deepseek-background"},
		},
	}
	cfg.Normalize()
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	app := &App{
		ctx: context.Background(), stdout: &stdout, stderr: &stderr,
		configPath: configPath, interactive: true,
	}
	got, code := app.ensureOnboarding(cfg, onboardingOptions{SkipGateway: true})
	if code != 0 || got == nil {
		t.Fatalf("missing readiness cfg=%v code=%d stdout=%q stderr=%q", got != nil, code, stdout.String(), stderr.String())
	}
	if !app.modelManagerOnly {
		t.Fatal("onboarding did not route missing readiness into the sole Model Manager")
	}
	if !strings.Contains(stdout.String(), "Opening Model Manager") || strings.Contains(stdout.String(), "Continue with these models?") {
		t.Fatalf("model manager handoff output = %q", stdout.String())
	}
	status, err := (&modelchange.Service{ConfigPath: configPath}).Inspect()
	if err != nil || status.ModelReady() {
		t.Fatalf("handoff established readiness before Model Manager apply: ready=%v err=%v", status.ModelReady(), err)
	}
}

func TestOnboardingWorkspaceRequiresProjectDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(home, "project")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if !onboardingWorkspaceNeedsExplicitChoice(home) || !onboardingWorkspaceNeedsExplicitChoice(string(filepath.Separator)) {
		t.Fatal("home and filesystem root must never be accepted as implicit workspaces")
	}
	if onboardingWorkspaceNeedsExplicitChoice(project) {
		t.Fatal("project directory should be accepted")
	}
}
