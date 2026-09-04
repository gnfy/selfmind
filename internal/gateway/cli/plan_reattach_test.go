package cli

import (
	"strings"
	"testing"

	"selfmind/internal/gateway/api"
)

// TestReattachRestoresThePinnedPlan pins the progress-visibility contract for a
// person coming back to work already in flight. Attaching told them WHAT was
// running but not HOW FAR along: the pinned plan is driven by live plan.updated
// events, and the next snapshot can be a long tool step away — or never — so
// the block stayed empty for exactly the runs where progress matters most.
func TestReattachRestoresThePinnedPlan(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.width = 100
	model.height = 30
	model.clientMode = true
	model.startupDigest = &api.DigestResponse{
		ActiveRun: &api.DigestActiveRun{
			RunID: "run_abc",
			Title: "gcp release",
			PlanJSON: `{"explanation":"release steps","plan":[` +
				`{"step":"pre-flight checks","status":"completed"},` +
				`{"step":"trigger builds","status":"in_progress"},` +
				`{"step":"backfill the release record","status":"pending"}]}`,
		},
	}

	model.maybeShowStartupDigest(model.width)

	if strings.TrimSpace(model.activePlanJSON) == "" {
		t.Fatal("attaching mid-run left the pinned plan empty")
	}
	rendered := stripANSI(model.viewActiveRegion())
	if !strings.Contains(rendered, "Plan · 1/3") {
		t.Fatalf("pinned plan should show progress after attach: %q", rendered)
	}
	for _, step := range []string{"pre-flight checks", "trigger builds", "backfill the release record"} {
		if !strings.Contains(rendered, step) {
			t.Fatalf("restored plan missing %q: %q", step, rendered)
		}
	}
}

// A digest with no plan must not fabricate one.
func TestReattachWithoutAPlanShowsNoPlanBlock(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.width = 100
	model.height = 30
	model.clientMode = true
	model.startupDigest = &api.DigestResponse{
		ActiveRun: &api.DigestActiveRun{RunID: "run_abc", Title: "no plan yet"},
	}

	model.maybeShowStartupDigest(model.width)

	if strings.TrimSpace(model.activePlanJSON) != "" {
		t.Fatalf("no plan in the digest should leave none pinned, got %q", model.activePlanJSON)
	}
}
