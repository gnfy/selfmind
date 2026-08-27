package modelchange

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfmind/internal/platform/config"
)

func TestPrepareConfirmAndStartupApply(t *testing.T) {
	service, path := newTestService(t)
	cfg := mustLoadConfig(t, path)
	candidate := SnapshotFromConfig(cfg)
	candidate.Primary.Provider = "codex-cli"
	candidate.Primary.Model = "gpt-next"
	candidate.Primary.Reasoning = "high"

	prepared, err := service.Prepare(context.Background(), PrepareRequest{
		Candidate: candidate, Source: "cli", RequireConfirmation: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.NeedsConfirm || prepared.Change.Status != StatusAwaitingConfirmation {
		t.Fatalf("prepared = %+v", prepared)
	}
	if got := SnapshotFromConfig(mustLoadConfig(t, path)); got == candidate {
		t.Fatal("preview mutated config before confirmation")
	}

	committed, err := service.Confirm(context.Background(), prepared.Change.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !committed.NeedsRestart || committed.Change.Status != StatusAwaitingSafeBoundary {
		t.Fatalf("committed = %+v", committed)
	}
	if got := SnapshotFromConfig(mustLoadConfig(t, path)); got != committed.Change.Previous {
		t.Fatalf("config changed before the safe boundary: got %+v, want %+v", got, committed.Change.Previous)
	}
	status, err := service.BeginDraining(committed.Change.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Pending == nil || status.Pending.Status != StatusDraining || status.Configured != candidate {
		t.Fatalf("draining status = %+v", status)
	}
	status, err = service.MarkRestarting(committed.Change.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if status.Pending == nil || status.Pending.Status != StatusRestarting {
		t.Fatalf("restarting status = %+v", status)
	}

	status, rolledBack, err := service.ReconcileStartup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack || status.Pending == nil || status.Pending.Status != StatusStarting || status.Running == candidate || status.Configured != candidate {
		t.Fatalf("startup status=%+v rolledBack=%v", status, rolledBack)
	}
	status, err = service.MarkStartupHealthy()
	if err != nil {
		t.Fatal(err)
	}
	if status.Pending != nil || status.Running != candidate || status.Configured != candidate {
		t.Fatalf("healthy startup status=%+v", status)
	}
	if len(status.History) != 1 || status.History[0].Status != StatusApplied {
		t.Fatalf("history = %+v", status.History)
	}
}

func TestModelReadinessRequiresVerifiedRunningBoundary(t *testing.T) {
	service, path := newUnverifiedTestService(t)
	status, err := service.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if status.ModelReady() {
		t.Fatal("fresh configured routes were ready before a verified running boundary")
	}
	status, rolledBack, err := service.ReconcileStartup(context.Background())
	if err != nil || rolledBack || status.Pending == nil || status.Pending.Status != StatusStarting {
		t.Fatalf("initial startup validation = status:%+v rolledBack:%v err:%v", status, rolledBack, err)
	}
	if len(status.Pending.Probes) < 2 {
		t.Fatalf("initial startup did not validate Main and Background: %+v", status.Pending.Probes)
	}
	status, err = service.MarkStartupHealthy()
	if err != nil {
		t.Fatal(err)
	}
	if !status.ModelReady() {
		t.Fatalf("healthy running routes were not ready: %+v", status)
	}
	cfg := mustLoadConfig(t, path)
	cfg.Models.Auxiliary.Model = "manual-drift"
	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	status, err = service.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if status.ModelReady() {
		t.Fatal("manual configuration drift retained Model Readiness")
	}
}

func TestMarkStartupHealthyCannotBlessAnUnvalidatedBaseline(t *testing.T) {
	service, _ := newUnverifiedTestService(t)
	if _, err := service.Inspect(); err != nil {
		t.Fatal(err)
	}
	status, err := service.MarkStartupHealthy()
	if err != nil {
		t.Fatal(err)
	}
	if status.ModelReady() || !status.RunningVerifiedAt.IsZero() {
		t.Fatalf("unvalidated baseline became ready: %+v", status)
	}
}

func TestInitialProbeFailureKeepsModelManagerRepairSurfaceAvailable(t *testing.T) {
	service, _ := newUnverifiedTestService(t)
	service.Validate = func(_ context.Context, _ *config.Config, routes []Route) []ProbeResult {
		results := make([]ProbeResult, 0, len(routes))
		for _, route := range routes {
			results = append(results, ProbeResult{Route: route, Error: "credential missing", FailureClass: FailureModel})
		}
		return results
	}
	status, rolledBack, err := service.ReconcileStartup(context.Background())
	if err != nil || rolledBack || status.Pending != nil || status.ModelReady() {
		t.Fatalf("failed initial probe = status:%+v rolledBack:%v err:%v", status, rolledBack, err)
	}
	if len(status.History) != 1 || status.History[0].Status != StatusFailed || status.History[0].Source != "initial-startup" {
		t.Fatalf("failed initial evidence = %+v", status.History)
	}
	candidate := status.Configured
	candidate.Primary.Model = "repair-candidate"
	if _, err := service.Prepare(context.Background(), PrepareRequest{Candidate: candidate, Source: "model-manager", RequireConfirmation: true}); err != nil {
		t.Fatalf("Model Manager could not open a repair transaction: %v", err)
	}
}

func TestCredentialOnlyRepairCanRevalidateUnchangedRoutes(t *testing.T) {
	service, _ := newUnverifiedTestService(t)
	status, err := service.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := service.Prepare(context.Background(), PrepareRequest{
		Candidate: status.Configured, Source: "model-manager", RequireConfirmation: false, ForceRevalidate: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.NeedsRestart || prepared.Change.Status != StatusAwaitingSafeBoundary || len(prepared.Change.ChangedRoutes) < 2 {
		t.Fatalf("credential-only repair = %+v", prepared)
	}
}

func TestSnapshotFingerprintTracksEffectiveRoutesWithoutSecrets(t *testing.T) {
	_, path := newTestService(t)
	cfg := mustLoadConfig(t, path)
	before := SnapshotFromConfig(cfg).Fingerprint()
	if before == "" {
		t.Fatal("model snapshot fingerprint is empty")
	}
	cfg.Providers.OpenAI.APIKey = "secret-that-must-not-affect-route-identity"
	if afterSecret := SnapshotFromConfig(cfg).Fingerprint(); afterSecret != before {
		t.Fatalf("credential changed route fingerprint: %q != %q", afterSecret, before)
	}
	cfg.Models.Auxiliary.Model = "different-background"
	if afterRoute := SnapshotFromConfig(cfg).Fingerprint(); afterRoute == before {
		t.Fatal("changed Background route retained the same fingerprint")
	}
}

func TestSchemaV2AppliedSnapshotMigratesAsReadyWithBackup(t *testing.T) {
	service, path := newTestService(t)
	cfg := mustLoadConfig(t, path)
	snapshot := SnapshotFromConfig(cfg)
	now := time.Now().UTC().Add(-time.Minute)
	legacy := State{
		SchemaVersion: 2, Generation: 7, Running: snapshot, UpdatedAt: now,
		History: []Change{{
			ID: "model_applied", Status: StatusApplied, Previous: snapshot, Candidate: snapshot,
			CreatedAt: now, FinishedAt: now,
		}},
	}
	data, err := json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(service.statePath(), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := service.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if !status.ModelReady() {
		t.Fatalf("applied schema-v2 snapshot did not migrate as ready: %+v", status)
	}
	backup, err := os.ReadFile(service.statePath() + ".v2.backup")
	if err != nil {
		t.Fatalf("schema-v2 backup: %v", err)
	}
	if !bytes.Equal(backup, append(data, '\n')) {
		t.Fatalf("schema-v2 backup changed:\n%s", backup)
	}
}

func TestRoleOverrideIsPartOfAtomicSnapshotWithoutDestroyingAdvancedConfig(t *testing.T) {
	service, path := newTestService(t)
	cfg := mustLoadConfig(t, path)
	cfg.Models.Roles = map[string]config.ModelRoleConfig{
		"memory_extract": {
			Provider: "deepseek", Model: "old-role", BaseURL: "https://example.invalid/v1",
			Protocol: "openai_compatible", ExtraHeaders: map[string]string{"X-Test": "preserve"},
		},
	}
	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	// Adopt the manual starting point before opening a managed transaction.
	service = &Service{ConfigPath: path, Validate: service.Validate}
	status, rolledBack, err := service.ReconcileStartup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack || status.Pending == nil {
		t.Fatalf("manual starting point was not validated: status=%+v rolledBack=%v", status, rolledBack)
	}
	status, err = service.MarkStartupHealthy()
	if err != nil {
		t.Fatal(err)
	}
	candidate := status.Configured
	candidate.Primary.Model = "gpt-next"
	candidate.Roles.MemoryExtract = config.ModelSelectionConfig{Provider: "anthropic", Model: "claude-role", Reasoning: "low"}

	prepared, err := service.Prepare(context.Background(), PrepareRequest{Candidate: candidate, Source: "cli", RequireConfirmation: true})
	if err != nil {
		t.Fatal(err)
	}
	wantRoutes := []Route{RouteMemoryExtract, RoutePrimary}
	if got := SortedRoutes(prepared.Change.ChangedRoutes); len(got) != len(wantRoutes) || got[0] != wantRoutes[0] || got[1] != wantRoutes[1] {
		t.Fatalf("changed routes = %v, want %v", got, wantRoutes)
	}
	if _, err := service.Confirm(context.Background(), prepared.Change.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.BeginDraining(prepared.Change.ID); err != nil {
		t.Fatal(err)
	}
	applied := mustLoadConfig(t, path).Models.Roles["memory_extract"]
	if applied.Provider != "anthropic" || applied.Model != "claude-role" || applied.Reasoning != "low" {
		t.Fatalf("role selection = %+v", applied)
	}
	if applied.BaseURL != "https://example.invalid/v1" || applied.Protocol != "openai_compatible" || applied.ExtraHeaders["x-test"] != "preserve" {
		t.Fatalf("advanced role config was destroyed: %+v", applied)
	}
}

func TestApplySnapshotMakesLegacyCodingAgentOverrideInert(t *testing.T) {
	cfg := &config.Config{Models: config.ModelsConfig{Roles: map[string]config.ModelRoleConfig{
		"coding_agent": {Provider: "deepseek", Model: "hidden-main"},
	}}}
	ApplySnapshot(cfg, Snapshot{Primary: config.ModelSelectionConfig{Provider: "codex-cli", Model: "gpt-main"}})
	if _, exists := cfg.Models.Roles["coding_agent"]; exists {
		t.Fatalf("selection-only coding_agent override survived: %+v", cfg.Models.Roles)
	}

	cfg.Models.Roles["coding_agent"] = config.ModelRoleConfig{
		Provider: "deepseek", Model: "hidden-main", BaseURL: "https://advanced.invalid/v1",
	}
	ApplySnapshot(cfg, Snapshot{Primary: config.ModelSelectionConfig{Provider: "codex-cli", Model: "gpt-main"}})
	legacy := cfg.Models.Roles["coding_agent"]
	if legacy.Provider != "" || legacy.Model != "" || legacy.BaseURL != "https://advanced.invalid/v1" {
		t.Fatalf("advanced legacy override was not safely neutralized: %+v", legacy)
	}
}

func TestConfirmIsIdempotentAfterReceiptWasLost(t *testing.T) {
	service, _ := newTestService(t)
	status, err := service.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	candidate := status.Running
	candidate.Primary.Model = "idempotent-next"
	prepared, err := service.Prepare(context.Background(), PrepareRequest{
		Candidate: candidate, Source: "http", RequireConfirmation: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.Confirm(context.Background(), prepared.Change.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Confirm(context.Background(), prepared.Change.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Change.ID != second.Change.ID || second.Change.Status != StatusAwaitingSafeBoundary || !second.NeedsRestart {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
}

func TestFailedPreCommitProbeLeavesRunningConfigUntouched(t *testing.T) {
	service, path := newTestService(t)
	service.Validate = func(_ context.Context, _ *config.Config, routes []Route) []ProbeResult {
		return []ProbeResult{{Route: routes[0], Error: "unauthorized"}}
	}
	before := SnapshotFromConfig(mustLoadConfig(t, path))
	candidate := before
	candidate.Primary.Model = "broken"
	prepared, err := service.Prepare(context.Background(), PrepareRequest{
		Candidate: candidate, Source: "weixin", RequireConfirmation: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Confirm(context.Background(), prepared.Change.ID); err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("confirm error = %v", err)
	}
	status, err := service.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if status.Pending != nil || status.Running != before || status.Configured != before {
		t.Fatalf("status = %+v", status)
	}
	if len(status.History) != 1 || status.History[0].Status != StatusFailed {
		t.Fatalf("history = %+v", status.History)
	}
}

func TestValidateCandidateReturnsEvidenceWithoutCreatingPendingChange(t *testing.T) {
	service, path := newTestService(t)
	before, err := service.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	candidate := before.Configured
	candidate.Auxiliary = config.ModelSelectionConfig{Provider: "deepseek", Model: "deepseek-background"}

	probes, err := service.ValidateCandidate(context.Background(), candidate, []Route{RouteAuxiliary})
	if err != nil {
		t.Fatal(err)
	}
	if len(probes) == 0 || !probes[0].OK {
		t.Fatalf("probes = %+v", probes)
	}
	after, err := service.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if after.Pending != nil || after.Generation != before.Generation || after.Configured != before.Configured {
		t.Fatalf("validation mutated state: before=%+v after=%+v", before, after)
	}
	if got := SnapshotFromConfig(mustLoadConfig(t, path)); got != before.Configured {
		t.Fatalf("validation wrote config: %+v", got)
	}
}

func TestPostRestartProbeFailureRollsBackCandidate(t *testing.T) {
	service, path := newTestService(t)
	before := SnapshotFromConfig(mustLoadConfig(t, path))
	candidate := before
	candidate.Auxiliary.Provider = "anthropic"
	candidate.Auxiliary.Model = "claude-broken"
	prepared, err := service.Prepare(context.Background(), PrepareRequest{
		Candidate: candidate, Source: "cli", RequireConfirmation: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Confirm(context.Background(), prepared.Change.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.BeginDraining(prepared.Change.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.MarkRestarting(prepared.Change.ID, "test"); err != nil {
		t.Fatal(err)
	}
	service.Validate = func(_ context.Context, _ *config.Config, routes []Route) []ProbeResult {
		return []ProbeResult{{Route: routes[0], Error: "daemon credential missing"}}
	}
	status, rolledBack, err := service.ReconcileStartup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !rolledBack || status.Running != before || status.Configured != before || status.Pending != nil {
		t.Fatalf("status=%+v rolledBack=%v", status, rolledBack)
	}
	if got := SnapshotFromConfig(mustLoadConfig(t, path)); got != before {
		t.Fatalf("config after rollback = %+v, want %+v", got, before)
	}
}

func TestPostRestartInfrastructureProbeFailureRequiresRecoveryWithoutRollback(t *testing.T) {
	service, path := newTestService(t)
	before := SnapshotFromConfig(mustLoadConfig(t, path))
	candidate := before
	candidate.Primary.Model = "candidate-during-outage"
	prepared, err := service.Prepare(context.Background(), PrepareRequest{
		Candidate: candidate, Source: "cli", RequireConfirmation: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Confirm(context.Background(), prepared.Change.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.BeginDraining(prepared.Change.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.MarkRestarting(prepared.Change.ID, "test"); err != nil {
		t.Fatal(err)
	}
	service.Validate = func(_ context.Context, _ *config.Config, routes []Route) []ProbeResult {
		return []ProbeResult{{Route: routes[0], Error: "provider temporarily unavailable", FailureClass: FailureInfrastructure}}
	}
	status, rolledBack, err := service.ReconcileStartup(context.Background())
	if err != nil {
		t.Fatalf("ReconcileStartup error = %v; recovery control plane should remain available", err)
	}
	if rolledBack || status.Pending == nil || status.Pending.Status != StatusRecoveryRequired || status.Running != before || status.Configured != candidate {
		t.Fatalf("status=%+v rolledBack=%v", status, rolledBack)
	}
	if got := SnapshotFromConfig(mustLoadConfig(t, path)); got != candidate {
		t.Fatalf("config after infrastructure failure = %+v, want candidate preserved", got)
	}
	status, rolledBack, err = service.ReconcileStartup(context.Background())
	if err != nil || rolledBack || status.Pending == nil || status.Pending.Status != StatusRecoveryRequired {
		t.Fatalf("recovery control plane restart = status:%+v rolledBack:%v err:%v", status, rolledBack, err)
	}
}

func TestManualConfigEditUsesStartupValidation(t *testing.T) {
	service, path := newTestService(t)
	before, err := service.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	cfg := mustLoadConfig(t, path)
	cfg.Models.Primary.Model = "manual-new"
	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	status, rolledBack, err := service.ReconcileStartup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack || status.Pending == nil || status.Running != before.Running || status.Configured.Primary.Model != "manual-new" {
		t.Fatalf("status=%+v rolledBack=%v", status, rolledBack)
	}
	status, err = service.MarkStartupHealthy()
	if err != nil {
		t.Fatal(err)
	}
	if status.Pending != nil || status.Running.Primary.Model != "manual-new" || status.Running == before.Running {
		t.Fatalf("healthy status=%+v", status)
	}
}

func TestDaemonConstructionFailureAfterProbeRequiresRecoveryWithoutModelRollback(t *testing.T) {
	service, path := newTestService(t)
	before := SnapshotFromConfig(mustLoadConfig(t, path))
	candidate := before
	candidate.Primary.Model = "passes-probe-but-cannot-start"
	prepared, err := service.Prepare(context.Background(), PrepareRequest{
		Candidate: candidate, Source: "cli", RequireConfirmation: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Confirm(context.Background(), prepared.Change.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.BeginDraining(prepared.Change.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.MarkRestarting(prepared.Change.ID, "test"); err != nil {
		t.Fatal(err)
	}
	status, rolledBack, err := service.ReconcileStartup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack || status.Pending == nil || status.Pending.Status != StatusStarting {
		t.Fatalf("status=%+v rolledBack=%v", status, rolledBack)
	}
	status, rolledBack, err = service.FailStarting(errors.New("listener bind failed"))
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack || status.Pending == nil || status.Pending.Status != StatusRecoveryRequired || status.Running != before || status.Configured != candidate {
		t.Fatalf("recovery status=%+v rolledBack=%v", status, rolledBack)
	}
	if !strings.Contains(status.Pending.Failure, "listener bind failed") || status.Pending.FailureClass != FailureInfrastructure {
		t.Fatalf("pending = %+v", status.Pending)
	}
}

func TestRecoveryCanRetryOrRestoreWithoutOverwritingDrift(t *testing.T) {
	service, path := newTestService(t)
	before := SnapshotFromConfig(mustLoadConfig(t, path))
	candidate := before
	candidate.Primary.Model = "recovery-candidate"
	prepared, err := service.Prepare(context.Background(), PrepareRequest{Candidate: candidate, Source: "cli", RequireConfirmation: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Confirm(context.Background(), prepared.Change.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.BeginDraining(prepared.Change.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.MarkRestarting(prepared.Change.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.ReconcileStartup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.FailStarting(errors.New("port unavailable")); err != nil {
		t.Fatal(err)
	}
	status, err := service.RetryRecovery(prepared.Change.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Pending == nil || status.Pending.Status != StatusRestarting || status.Pending.RestartAttempts != 0 {
		t.Fatalf("retry status = %+v", status)
	}
	if _, _, err := service.ReconcileStartup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.FailStarting(errors.New("still unavailable")); err != nil {
		t.Fatal(err)
	}
	status, err = service.RestorePrevious(prepared.Change.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Pending != nil || status.Configured != before || status.Running != before || status.ModelReady() {
		t.Fatalf("restore status = %+v", status)
	}
	if got := status.History[len(status.History)-1]; got.Status != StatusRolledBack {
		t.Fatalf("history = %+v", status.History)
	}
	status, rolledBack, err := service.ReconcileStartup(context.Background())
	if err != nil || rolledBack || status.Pending == nil || status.Pending.Status != StatusStarting {
		t.Fatalf("restored startup status=%+v rolledBack=%v err=%v", status, rolledBack, err)
	}
}

func TestSchemaV1IsBackedUpAndMigrated(t *testing.T) {
	service, path := newTestService(t)
	status, err := service.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	statePath := service.StatePath()
	legacy := State{
		SchemaVersion: 1, Generation: status.Generation, Running: status.Running,
		Pending: &Change{ID: "legacy", Status: ChangeStatus("awaiting_restart"), Previous: status.Running, Candidate: status.Running, CreatedAt: time.Now()},
	}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	migrated, err := (&Service{ConfigPath: path}).Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if migrated.Pending == nil || migrated.Pending.Status != StatusAwaitingSafeBoundary {
		t.Fatalf("migrated = %+v", migrated)
	}
	backup, err := os.ReadFile(statePath + ".v1.backup")
	if err != nil {
		t.Fatalf("schema backup missing: %v", err)
	}
	if !bytes.Equal(backup, data) {
		t.Fatalf("schema-v1 backup changed:\n%s", backup)
	}
}

func TestCancelRestoresCommittedButNotAppliedCandidate(t *testing.T) {
	service, path := newTestService(t)
	before := SnapshotFromConfig(mustLoadConfig(t, path))
	candidate := before
	candidate.Primary.Model = "cancel-me"
	prepared, err := service.Prepare(context.Background(), PrepareRequest{
		Candidate: candidate, Source: "cli", RequireConfirmation: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Confirm(context.Background(), prepared.Change.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Cancel(prepared.Change.ID); err != nil {
		t.Fatal(err)
	}
	status, err := service.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if status.Configured != before || status.Pending != nil {
		t.Fatalf("status = %+v", status)
	}
	if got := status.History[len(status.History)-1].Status; got != StatusCancelled {
		t.Fatalf("last history status = %s", got)
	}
}

func TestGenerationAndPendingConflictAreExplicit(t *testing.T) {
	service, path := newTestService(t)
	status, err := service.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	candidate := status.Configured
	candidate.Primary.Model = "first"
	if _, err := service.Prepare(context.Background(), PrepareRequest{
		Candidate: candidate, Source: "cli", ExpectedGeneration: status.Generation,
		RequireConfirmation: true,
	}); err != nil {
		t.Fatal(err)
	}
	candidate.Primary.Model = "second"
	_, err = service.Prepare(context.Background(), PrepareRequest{
		Candidate: candidate, Source: "weixin", ExpectedGeneration: status.Generation,
		RequireConfirmation: true,
	})
	if err == nil || (!errors.Is(err, ErrGenerationConflict) && !strings.Contains(err.Error(), "already")) {
		t.Fatalf("conflict error = %v", err)
	}
	_ = path
}

func TestConfirmationExpiresWithoutConfigChange(t *testing.T) {
	service, path := newTestService(t)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	service.Now = func() time.Time { return now }
	service.ConfirmTTL = time.Minute
	before := SnapshotFromConfig(mustLoadConfig(t, path))
	candidate := before
	candidate.Primary.Model = "late"
	prepared, err := service.Prepare(context.Background(), PrepareRequest{
		Candidate: candidate, Source: "weixin", RequireConfirmation: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := service.Confirm(context.Background(), prepared.Change.ID); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("confirm error = %v", err)
	}
	if got := SnapshotFromConfig(mustLoadConfig(t, path)); got != before {
		t.Fatalf("config = %+v, want %+v", got, before)
	}
}

func TestInspectExpiresStalePreviewAndUnblocksNextChange(t *testing.T) {
	service, path := newTestService(t)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	service.Now = func() time.Time { return now }
	service.ConfirmTTL = time.Minute
	before := SnapshotFromConfig(mustLoadConfig(t, path))
	first := before
	first.Primary.Model = "expired-preview"
	if _, err := service.Prepare(context.Background(), PrepareRequest{
		Candidate: first, Source: "weixin", RequireConfirmation: true,
	}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	status, err := service.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if status.Pending != nil || len(status.History) != 1 || status.History[0].Failure != "confirmation expired" {
		t.Fatalf("status = %+v", status)
	}
	second := before
	second.Primary.Model = "next-preview"
	if _, err := service.Prepare(context.Background(), PrepareRequest{
		Candidate: second, Source: "weixin", ExpectedGeneration: status.Generation, RequireConfirmation: true,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStartupLeavesUnconfirmedPreviewPending(t *testing.T) {
	service, path := newTestService(t)
	before := SnapshotFromConfig(mustLoadConfig(t, path))
	candidate := before
	candidate.Primary.Model = "preview-only"
	prepared, err := service.Prepare(context.Background(), PrepareRequest{
		Candidate: candidate, Source: "weixin", RequireConfirmation: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	status, rolledBack, err := service.ReconcileStartup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack || status.Pending == nil || status.Pending.ID != prepared.Change.ID || status.Pending.Status != StatusAwaitingConfirmation {
		t.Fatalf("status=%+v rolledBack=%v", status, rolledBack)
	}
	if status.Running != before || status.Configured != before {
		t.Fatalf("preview changed route state: %+v", status)
	}
}

func TestPrepareRejectsUnvalidatedManualConfigDrift(t *testing.T) {
	service, path := newTestService(t)
	if _, err := service.Inspect(); err != nil {
		t.Fatal(err)
	}
	cfg := mustLoadConfig(t, path)
	cfg.Models.Primary.Model = "manual-unvalidated"
	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	status, err := service.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	candidate := status.Configured
	candidate.Auxiliary.Model = "another-change"
	if _, err := service.Prepare(context.Background(), PrepareRequest{
		Candidate: candidate, Source: "cli", RequireConfirmation: true,
	}); err == nil || !strings.Contains(err.Error(), "manual edit") {
		t.Fatalf("prepare error = %v", err)
	}
}

func newTestService(t *testing.T) (*Service, string) {
	t.Helper()
	service, path := newUnverifiedTestService(t)
	if _, err := service.AcceptMigrationReadiness(); err != nil {
		t.Fatal(err)
	}
	return service, path
}

func newUnverifiedTestService(t *testing.T) (*Service, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.LoadConfig(config.Options{Path: path, CreateIfMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	cfg.SetPrimaryModel("codex-cli", "gpt-current", "medium")
	cfg.Models.Auxiliary = config.ModelSelectionConfig{Provider: "codex-cli", Model: "gpt-background", Reasoning: "low"}
	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		ConfigPath: path,
		Validate: func(_ context.Context, cfg *config.Config, routes []Route) []ProbeResult {
			results := make([]ProbeResult, 0, len(routes))
			for _, route := range routes {
				selection := SnapshotFromConfig(cfg).Primary
				if route == RouteAuxiliary {
					selection = SnapshotFromConfig(cfg).Auxiliary
				}
				results = append(results, ProbeResult{Route: route, OK: true, Provider: selection.Provider, Model: selection.Model})
			}
			return results
		},
	}
	return service, path
}

func mustLoadConfig(t *testing.T, path string) *config.Config {
	t.Helper()
	cfg, err := config.LoadConfig(config.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
