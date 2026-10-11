package eval

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The release case drives /effects and /effects settle against an effect the
// harness left uncertain through the production claim path, without a model.
func TestExternalEffectSettleCasePassesOffline(t *testing.T) {
	if testing.Short() {
		t.Skip("boots the full gateway harness")
	}
	_, cfgPath := writeRunnerFixtures(t)
	result, err := RunCaseFile(context.Background(), filepath.Join("..", "..", "evalcases", "continuity", "external-effect-settle.yaml"),
		RunOptions{ConfigPath: cfgPath, OutputPath: filepath.Join(t.TempDir(), "out.jsonl")})
	if err != nil {
		t.Fatalf("RunCaseFile: %v", err)
	}
	if failed := failedChecks(result); len(failed) != 0 {
		t.Fatalf("external_effect_settle failed: %v", failed)
	}
	for _, name := range []string{"contains:Settled external effect", "contains:was already settled", "contains:belongs to you."} {
		if check, ok := checkByName(result, name); !ok || !check.OK {
			t.Fatalf("check %s did not run or failed: %+v", name, check)
		}
	}
}

// seeded_effect_ordinal names only a parked Run that carries an effect.
func TestSeededEffectOrdinalMustNameAnEffect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "case.yaml")
	if err := os.WriteFile(path, []byte(`id: effect_ordinal_without_effect
title: "ordinal without an effect"
channel: cli
model_required: false
setup:
  task:
    parked_runs:
      - input: "parked"
turns:
  - input: "/effects settle"
    seeded_effect_ordinal: 1
expect:
  max_duration_seconds: 30
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCase(path); err == nil || !strings.Contains(err.Error(), "seeded_effect_ordinal") {
		t.Fatalf("an ordinal without an unresolved_effect loaded: %v", err)
	}
}
