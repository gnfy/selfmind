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
	casePath := filepath.Join(t.TempDir(), "external-effect-settle.yaml")
	if err := os.WriteFile(casePath, []byte(`id: external_effect_settle
title: "an uncertain external effect is listed with its release paths and settled only by its own person"
suite: continuity
workspace: isolated
channel: cli
model_required: false
setup:
  task:
    title: "Publish the release"
    parked_runs:
      - input: "Publish version 2.1.0 to the registry"
        status: blocked
        unresolved_effect:
          summary: "npm publish --tag next"
          exit_code: 1
turns:
  - input: "/effects"
  - input: "/effects settle"
  - input: "/effects settle"
    platform_user_id: "eval-stranger"
    seeded_effect_ordinal: 1
  - input: "/effects settle"
    seeded_effect_ordinal: 1
  - input: "/effects settle"
    seeded_effect_ordinal: 1
  - input: "/effects"
expect:
  status: completed
  max_tool_calls: 0
  max_tool_errors: 0
  max_duration_seconds: 30
  contains:
    - "Unresolved external effects:"
    - "| uncertain | `+"`npm publish --tag next`"+`, exit 1"
    - "unknown; conflicts with all your external targets"
    - "A one-time approval leaves the original claim unresolved"
    - "later conflicting actions may ask again"
    - "| since "
    - "settle it with /effects settle <claim_id> [reason]"
    - "Usage: /effects [settle <claim_id> [reason] | resolve <claim_id> <watch_id>]"
    - "belongs to you."
    - "Settled external effect"
    - "was already settled"
    - "No unresolved external effects."
    - "Age and approval of a different action never release an uncertain effect"
checks:
  no_mojibake: true
  no_raw_json_leak: true
  no_empty_response: true
  no_provider_stack_dump: true
`), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := RunCaseFile(context.Background(), casePath,
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
