package app

import (
	"encoding/json"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/platform/config"
	"selfmind/internal/tools"
)

// TestReviewedDeferralShrinksTheModelToolSurface is the application wiring
// boundary for the reviewed deferral cohort. It pins three things that a future
// cohort edit must not break: the cohort actually reaches the model surface,
// the tools that must never be deferred stay direct, and the saving is real.
//
// The 20% share target in the active plan is NOT met by this cohort alone —
// measured saving is about a quarter of the catalogue — so the assertion is the
// measured floor, not the target. Claiming the target here would turn an
// unfinished job into a green test.
func TestReviewedDeferralShrinksTheModelToolSurface(t *testing.T) {
	base := t.TempDir()
	cfg := &config.Config{Evolution: config.EvolutionConfig{SkillsDir: base}}
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	dispatcher, err := InitTools(nil, cfg, nil, "default", nil, store)
	if err != nil {
		t.Fatal(err)
	}

	all := dispatcher.GetToolDefinitions()
	exposure := map[string]string{}
	direct := make([]map[string]interface{}, 0, len(all))
	for _, definition := range all {
		name := ""
		if fn, ok := definition["function"].(map[string]interface{}); ok {
			name, _ = fn["name"].(string)
		}
		kind := ""
		if metadata, ok := definition["selfmind"].(map[string]interface{}); ok {
			kind, _ = metadata["exposure"].(string)
		}
		exposure[name] = kind
		if kind != string(tools.ToolExposureDeferred) {
			direct = append(direct, definition)
		}
	}

	// The reviewed cohort must be in effect, not merely declared.
	for _, name := range []string{
		"skill_manage", "skill_bundle", "skill_catalog", "skills_list", "skill_view",
		"text_to_speech", "vision_analyze", "execute_code", "delegate_task", "process", "web_search",
	} {
		if got := exposure[name]; got != string(tools.ToolExposureDeferred) {
			t.Errorf("%s must be deferred, exposure=%q", name, got)
		}
	}

	// These must stay in every request. Rarity is not coldness: asking the
	// person, discovering tools, reading back a large result, and finding the
	// work a turn belongs to all have to be reachable without a round trip.
	for _, name := range []string{
		"tool_search", "clarify", "request_permissions", "tool_output_view", "write_file",
		"session_search", "work_search", "work_inspect", "work_select",
		"queue_user_input", "set_delivery_target", "web_extract",
		"terminal", "patch", "read_file", "batch_read", "update_plan", "finish_run",
	} {
		if got := exposure[name]; got == string(tools.ToolExposureDeferred) {
			t.Errorf("%s must never be deferred", name)
		}
	}

	rawAll, err := json.Marshal(all)
	if err != nil {
		t.Fatal(err)
	}
	rawDirect, err := json.Marshal(direct)
	if err != nil {
		t.Fatal(err)
	}
	saved := len(rawAll) - len(rawDirect)
	if share := 100 * float64(saved) / float64(len(rawAll)); share < 20 {
		t.Fatalf("reviewed deferral saves only %.1f%% of the tool surface (%d of %d bytes); the cohort regressed",
			share, saved, len(rawAll))
	}
}
