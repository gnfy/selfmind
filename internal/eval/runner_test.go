package eval

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/platform/config"
	"selfmind/internal/tools"
)

// writeRunnerFixtures prepares a hermetic eval environment: temp HOME (no real
// CLI auth or skills), a config whose data dir points at a temp "real" dir,
// and strict offline VCR so no provider call can ever leave the process.
// It returns the configured ("real") data dir and the config path.
func writeRunnerFixtures(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SELF_TENANT_ID", "")
	t.Setenv("SELFMIND_EVAL_VCR", "replay")
	t.Setenv("SELFMIND_EVAL_OFFLINE", "1")
	t.Setenv("SELFMIND_EVAL_VCR_DIR", filepath.Join(t.TempDir(), "vcr"))

	realDataDir := filepath.Join(t.TempDir(), "realdata")
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := "storage:\n  type: \"sqlite\"\n  data_dir: " + strconv.Quote(realDataDir) + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return realDataDir, cfgPath
}

func writeRunnerCase(t *testing.T, id string, extra string) *Case {
	t.Helper()
	content := "id: " + id + "\n" +
		"title: \"runner isolation test\"\n" +
		"channel: cli\n" +
		extra +
		"turns:\n  - input: \"hello\"\n" +
		"expect:\n  max_duration_seconds: 60\n"
	path := filepath.Join(t.TempDir(), id+".yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write case: %v", err)
	}
	c, err := LoadCase(path)
	if err != nil {
		t.Fatalf("load case: %v", err)
	}
	return c
}

func TestApplyEvalModelOverrideUsesCanonicalPrimarySelection(t *testing.T) {
	cfg := &config.Config{
		Models: config.ModelsConfig{
			Primary: config.ModelSelectionConfig{
				Provider:  "codex-cli",
				Model:     "gpt-5.6-sol",
				Reasoning: "high",
			},
		},
		Model: config.ModelConfig{Provider: "legacy", Default: "legacy-model"},
	}
	applyEvalModelOverride(cfg, "openrouter", "nemotron-test")
	got := cfg.EffectivePrimary()
	if got.Provider != "openrouter" || got.Model != "nemotron-test" {
		t.Fatalf("primary override = %+v", got)
	}
	if cfg.Model.Provider != "" || cfg.Model.Default != "" {
		t.Fatalf("legacy selection survived canonical override: %+v", cfg.Model)
	}
	if got := cfg.EffectiveAuxiliary(); got.Provider != "openrouter" || got.Model != "nemotron-test" {
		t.Fatalf("auxiliary override = %+v", got)
	}
	if len(cfg.Models.Roles) != 0 {
		t.Fatalf("role-specific routes survived deterministic eval override: %+v", cfg.Models.Roles)
	}
}

func TestMakeEvalTempRootAvoidsSystemTemp(t *testing.T) {
	root, err := makeEvalTempRoot("scratch-root")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if pathWithin(os.TempDir(), root) {
		t.Fatalf("eval root %s must not live under system temp %s", root, os.TempDir())
	}
}

func TestApplyEvalModelOverridePreservesProviderForModelOnlyOverride(t *testing.T) {
	cfg := &config.Config{Models: config.ModelsConfig{Primary: config.ModelSelectionConfig{
		Provider: "deepseek", Model: "old-model", Reasoning: "high",
	}}}
	applyEvalModelOverride(cfg, "", "new-model")
	got := cfg.EffectivePrimary()
	if got.Provider != "deepseek" || got.Model != "new-model" || got.Reasoning != "high" {
		t.Fatalf("model-only override = %+v", got)
	}
}

func TestEvalTurnVCRContextDoesNotLeakIntoFlightRecording(t *testing.T) {
	t.Setenv("SELFMIND_FLIGHT_RECORDER", "1")
	t.Setenv("SELFMIND_EVAL_VCR", "")
	ctx := evalTurnVCRContext(context.Background(), "skill_case", "/workspace")
	if got := llm.VCRSessionForTest(ctx); got != "" {
		t.Fatalf("live eval leaked flight session %q", got)
	}

	t.Setenv("SELFMIND_EVAL_VCR", "replay")
	ctx = evalTurnVCRContext(context.Background(), "skill_case", "/workspace")
	if got := llm.VCRSessionForTest(ctx); got != "skill_case" {
		t.Fatalf("explicit eval replay session = %q, want skill_case", got)
	}
}

// TestRunCaseDefaultsToIsolatedDataDir is the regression test for the eval
// pollution bug: a plain case (no setup/assert_state/workspace:isolated) must
// NOT touch the configured data dir — its control.db/memory go to a throwaway
// temp dir, so no eval-* persons or running runs can leak into real data.
func TestRunCaseDefaultsToIsolatedDataDir(t *testing.T) {
	if testing.Short() {
		t.Skip("boots the full gateway harness")
	}
	realDataDir, cfgPath := writeRunnerFixtures(t)
	c := writeRunnerCase(t, "runner_isolation_default", "")
	beforeEvalDirs := homeEvalResidueDirs(t)
	beforeScratchDirs := cwdEvalTempRoots(t)

	out := filepath.Join(t.TempDir(), "out.jsonl")
	result, err := RunCase(context.Background(), c, RunOptions{ConfigPath: cfgPath, OutputPath: out})
	if err != nil {
		t.Fatalf("RunCase: %v", err)
	}
	if result == nil {
		t.Fatal("expected a result")
	}
	// The strongest possible assertion: nothing ever opened the configured data
	// dir, so control.db must not even exist there.
	if _, err := os.Stat(filepath.Join(realDataDir, "control.db")); !os.IsNotExist(err) {
		t.Fatalf("default eval run must not create the configured control.db (stat err=%v)", err)
	}
	// Storage isolation must cover both the stable eval tenant and the random
	// person id minted by the temporary control database. The old regression
	// assertion checked only eval-* and therefore missed person_* leaks.
	for name := range homeEvalResidueDirs(t) {
		if _, existed := beforeEvalDirs[name]; !existed {
			t.Fatalf("eval run leaked %q into the home .selfmind dir", name)
		}
	}
	for name := range cwdEvalTempRoots(t) {
		if _, existed := beforeScratchDirs[name]; !existed {
			t.Fatalf("eval run leaked temporary root %q into the package directory", name)
		}
	}
}

func homeEvalResidueDirs(t *testing.T) map[string]struct{} {
	t.Helper()
	result := map[string]struct{}{}
	home, err := os.UserHomeDir()
	if err != nil {
		return result
	}
	entries, err := os.ReadDir(filepath.Join(home, ".selfmind"))
	if err != nil {
		return result
	}
	for _, entry := range entries {
		if entry.IsDir() && (strings.HasPrefix(entry.Name(), "eval-") || strings.HasPrefix(entry.Name(), "person_")) {
			result[entry.Name()] = struct{}{}
		}
	}
	return result
}

func cwdEvalTempRoots(t *testing.T) map[string]struct{} {
	t.Helper()
	result := map[string]struct{}{}
	entries, err := os.ReadDir(".")
	if err != nil {
		return result
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), ".selfmind-eval-") {
			result[entry.Name()] = struct{}{}
		}
	}
	return result
}

func TestCleanupEvalTempRootRemovesOwnedTree(t *testing.T) {
	root := filepath.Join(t.TempDir(), "selfmind-eval-cleanup")
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "data", "control.db-wal"), []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupEvalTempRoot(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("temporary root remains: %v", err)
	}
}

func TestCleanupEvalTempRootRemovesReadOnlyToolchainTree(t *testing.T) {
	root := filepath.Join(t.TempDir(), "selfmind-eval-readonly-toolchain")
	moduleDir := filepath.Join(root, "runtime", "leases", "lease-test", "toolchain", "go-mod", "example.com", "module@v1.0.0")
	if err := os.MkdirAll(moduleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(moduleDir, "module.go"), []byte("package module\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(moduleDir, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := cleanupEvalTempRoot(root); err != nil {
		_ = os.Chmod(moduleDir, 0o700)
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("temporary root remains: %v", err)
	}
}

// TestRunCaseSharedDataOptsIntoConfiguredDir covers the explicit escape hatch:
// `shared_data: true` runs against the configured data dir, and the residue it
// leaves is exactly what `selfmind eval clean` selects.
func TestRunCaseSharedDataOptsIntoConfiguredDir(t *testing.T) {
	if testing.Short() {
		t.Skip("boots the full gateway harness")
	}
	realDataDir, cfgPath := writeRunnerFixtures(t)
	c := writeRunnerCase(t, "runner_isolation_shared", "shared_data: true\n")

	out := filepath.Join(t.TempDir(), "out.jsonl")
	if _, err := RunCase(context.Background(), c, RunOptions{ConfigPath: cfgPath, OutputPath: out}); err != nil {
		t.Fatalf("RunCase: %v", err)
	}
	store, err := control.OpenStore(realDataDir)
	if err != nil {
		t.Fatalf("open configured store: %v", err)
	}
	defer store.Close()
	report, err := store.CleanEvalResidue(context.Background(), false)
	if err != nil {
		t.Fatalf("residue dry run: %v", err)
	}
	if report.Persons != 1 || report.Accounts != 1 {
		t.Fatalf("shared_data run should leave exactly one eval identity in the configured store, got %+v", report)
	}
	// Run finalization: even in shared mode no run may linger as `running`.
	// MarkInterruptedRuns(0) flips every still-running run store-wide and
	// reports how many it found — which must be zero here.
	stuck, err := store.MarkInterruptedRuns(context.Background(), 0)
	if err != nil {
		t.Fatalf("scan for stuck runs: %v", err)
	}
	if stuck != 0 {
		t.Fatalf("shared_data run left %d run(s) in running status", stuck)
	}
}

func TestRunCaseExpectedGatewayRejectionPassesWithoutProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("boots the full gateway harness")
	}
	_, cfgPath := writeRunnerFixtures(t)
	maxTools := 0
	c := &Case{
		ID:      "unresolved_paste_rejected",
		Title:   "unresolved paste is rejected before dispatch",
		Channel: "cli",
		Turns: []Turn{{
			Input: "inspect this\n[Paste #1 · 80 lines]",
		}},
		Expect: Expectations{
			HTTPStatus:         400,
			RequireNoTask:      true,
			RequireNoRun:       true,
			MaxToolCalls:       &maxTools,
			MaxDurationSeconds: 10,
		},
		Checks: CheckSettings{
			NoProviderStackDump: true,
			ContextNotExceeded:  true,
		},
	}

	result, err := RunCase(context.Background(), c, RunOptions{
		ConfigPath: cfgPath,
		OutputPath: filepath.Join(t.TempDir(), "out.jsonl"),
	})
	if err != nil {
		t.Fatalf("RunCase: %v", err)
	}
	if result.Status != "passed" || !ChecksPassed(result.Checks) {
		t.Fatalf("expected gateway rejection to pass the eval: %+v", result)
	}
	if result.InputTokens != 0 || result.OutputTokens != 0 || result.ActionToolCalls != 0 {
		t.Fatalf("rejected input reached provider or tools: %+v", result)
	}
}

// TestIsolatedEvalConfigKeepsSkillsUnderTempDir is the regression test for the
// SkillsDir isolation leak: the harness overrode Storage.DataDir but left
// Evolution.SkillsDir empty, so app wiring fell back to ~/.selfmind and minted
// one `eval-<case>-<nano>/skills` home-dir directory per eval case. The
// resolved skills dir must live under the throwaway data dir, never under the
// user's home.
func TestIsolatedEvalConfigKeepsSkillsUnderTempDir(t *testing.T) {
	cfg := &config.Config{}
	tempData := filepath.Join(t.TempDir(), "data")
	isolatedEvalConfig(cfg, tempData)

	if cfg.Storage.DataDir != tempData {
		t.Fatalf("data dir override lost: %q", cfg.Storage.DataDir)
	}
	// Replicate app wiring's resolution (internal/app/agent.go): an empty
	// SkillsDir falls back to <home>/.selfmind before the tenant dir is minted.
	base := cfg.Evolution.SkillsDir
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".selfmind")
	}
	skillsDir := tools.SkillsDirForTenant(base, "eval-some_case-1234567890123456789")
	if !strings.HasPrefix(skillsDir, tempData+string(filepath.Separator)) {
		t.Fatalf("skills dir %q escaped the temp data dir %q", skillsDir, tempData)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if strings.HasPrefix(skillsDir, filepath.Join(home, ".selfmind")+string(filepath.Separator)) {
			t.Fatalf("skills dir %q resolved under the user home", skillsDir)
		}
	}

	// A blank data dir preserves configured durable paths for shared_data, but
	// operator identity must still be removed from release evidence.
	shared := &config.Config{}
	shared.Agent.Soul = "operator identity"
	isolatedEvalConfig(shared, "")
	if shared.Storage.DataDir != "" || shared.Evolution.SkillsDir != "" {
		t.Fatalf("shared_data config must be untouched, got %+v", shared)
	}
	if shared.Agent.Soul != "" {
		t.Fatalf("shared_data eval retained operator soul %q", shared.Agent.Soul)
	}
}

// TestFinalizeLeftoverRuns proves the harness force-finalizes any run that is
// still `running` after a case, instead of leaving phantom rows behind.
func TestFinalizeLeftoverRuns(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "eval-tenant", "eval", "eval-stuck", "SelfMind Eval")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "stuck", Channel: "cli"})
	if err != nil {
		t.Fatalf("task: %v", err)
	}
	if _, err := store.StartRun(ctx, task, "cli", "input"); err != nil {
		t.Fatalf("run: %v", err)
	}

	forced := finalizeLeftoverRuns(ctx, store, identity.TenantID, map[string]bool{identity.PersonID: true}, 100*time.Millisecond)
	if forced != 1 {
		t.Fatalf("expected 1 forced run, got %d", forced)
	}
	left, err := store.ListRunningRuns(ctx, identity.TenantID, nil)
	if err != nil {
		t.Fatalf("list running: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("run should be terminal after finalize, got %+v", left)
	}
	got, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil || got == nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != "interrupted" {
		t.Fatalf("task should be interrupted, got %q", got.Status)
	}
}
