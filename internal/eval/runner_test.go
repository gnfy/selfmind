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
	// SkillsDir isolation: app wiring must not mint an eval-tenant skills dir
	// under the (temp) home — that was the per-case ~/.selfmind/eval-*/skills
	// leak. writeRunnerFixtures pinned HOME, so any eval-* child here is a leak.
	home, _ := os.UserHomeDir()
	entries, _ := os.ReadDir(filepath.Join(home, ".selfmind"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "eval-") {
			t.Fatalf("eval run leaked %q into the home .selfmind dir", e.Name())
		}
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

	// A blank data dir must stay a no-op: the shared_data escape hatch runs
	// against the configured paths and must not gain surprise overrides.
	shared := &config.Config{}
	isolatedEvalConfig(shared, "")
	if shared.Storage.DataDir != "" || shared.Evolution.SkillsDir != "" {
		t.Fatalf("shared_data config must be untouched, got %+v", shared)
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
