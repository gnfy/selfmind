package kernel

import (
	"context"
	"fmt"
	"testing"

	"selfmind/internal/kernel/llm"
)

func searchResult(names ...string) string {
	out := "["
	for i, name := range names {
		if i > 0 {
			out += ","
		}
		out += `{"name":"` + name + `","activated":true}`
	}
	return out + "]"
}

func planProjection(sequence int) string {
	return fmt.Sprintf(`{"work_units":[{"sequence":%d,"plan_status":"in_progress"}]}`, sequence)
}

// TestActivationSurvivesResumeFromReplayedLedger pins durability. Activation
// lives in the run's context, so a run that discovered capabilities, parked on
// an approval, and resumed in a fresh RunConversation started with an empty set
// and refused every tool it had already activated. The tool_search results the
// loop checkpoint replays verbatim are the durable record.
func TestActivationSurvivesResumeFromReplayedLedger(t *testing.T) {
	original := withToolActivationState(context.Background())
	activateToolsFromSearchResult(original, "tool_search", searchResult("deploy_release", "rotate_secret"))
	if activatedToolCount(original) != 2 {
		t.Fatalf("activated=%d", activatedToolCount(original))
	}

	replayed := []llm.Message{
		{Role: "user", Content: "deploy the release"},
		{Role: "assistant", Content: "searching for deployment tools"},
		{Role: "tool", Name: "tool_search", Content: searchResult("deploy_release", "rotate_secret")},
		{Role: "tool", Name: "terminal", Content: "ok"},
	}
	resumed := withToolActivationState(context.Background())
	if deferredToolActive(resumed, "deploy_release") {
		t.Fatal("a fresh run must start with no activations")
	}
	if restored := seedToolActivationFromMessages(resumed, replayed); restored != 2 {
		t.Fatalf("restored=%d, want 2", restored)
	}
	for _, name := range []string{"deploy_release", "rotate_secret"} {
		if !deferredToolActive(resumed, name) {
			t.Errorf("%s was not restored after resume", name)
		}
	}
	if deferredToolActive(resumed, "never_activated") {
		t.Error("seeding must not activate tools the ledger never activated")
	}
}

// TestActivationResetsAtWorkUnitBoundary pins the scope. Within one unit the set
// only grows so the provider tools block stays stable; crossing into a new
// top-level unit clears it, mirroring Active Skill expiry.
func TestActivationResetsAtWorkUnitBoundary(t *testing.T) {
	ctx := withToolActivationState(context.Background())

	// First unit: entering it must not clear anything, and activation sticks
	// across further calls inside the same unit.
	if cleared := applyWorkUnitBoundary(ctx, 1); cleared != 0 {
		t.Fatalf("entering the first unit cleared %d", cleared)
	}
	activateToolsFromSearchResult(ctx, "tool_search", searchResult("deploy_release"))
	if cleared := applyWorkUnitBoundary(ctx, 1); cleared != 0 {
		t.Fatalf("staying in the same unit cleared %d", cleared)
	}
	if !deferredToolActive(ctx, "deploy_release") {
		t.Fatal("activation must persist inside one work unit")
	}

	// Second unit: the set is discarded.
	if cleared := applyWorkUnitBoundary(ctx, 2); cleared != 1 {
		t.Fatalf("crossing to unit 2 cleared %d, want 1", cleared)
	}
	if deferredToolActive(ctx, "deploy_release") {
		t.Fatal("activation leaked across a work-unit boundary")
	}
	if activatedToolCount(ctx) != 0 {
		t.Fatalf("active total=%d after reset", activatedToolCount(ctx))
	}
}

// TestSeedRestoresWorkUnitSoNextPlanIsNotABoundary guards the interaction
// between the two fixes: if a resume forgot which unit the restored set belongs
// to, the first update_plan afterwards would read as a transition and discard
// the work just restored.
func TestSeedRestoresWorkUnitSoNextPlanIsNotABoundary(t *testing.T) {
	replayed := []llm.Message{
		{Role: "tool", Name: "update_plan", Content: planProjection(3)},
		{Role: "tool", Name: "tool_search", Content: searchResult("deploy_release")},
	}
	ctx := withToolActivationState(context.Background())
	if restored := seedToolActivationFromMessages(ctx, replayed); restored != 1 {
		t.Fatalf("restored=%d", restored)
	}
	if cleared := applyWorkUnitBoundary(ctx, 3); cleared != 0 {
		t.Fatalf("the unit the restored set belongs to must not read as a boundary, cleared %d", cleared)
	}
	if !deferredToolActive(ctx, "deploy_release") {
		t.Fatal("restored activation was discarded by its own work unit")
	}
	if cleared := applyWorkUnitBoundary(ctx, 4); cleared != 1 {
		t.Fatalf("a genuine later boundary must still clear, cleared %d", cleared)
	}
}

func TestSeedRestoresOnlyLatestWorkUnitActivations(t *testing.T) {
	replayed := []llm.Message{
		{Role: "tool", Name: "update_plan", Content: planProjection(1)},
		{Role: "tool", Name: "tool_search", Content: searchResult("old_unit_tool")},
		{Role: "tool", Name: "update_plan", Content: planProjection(2)},
		{Role: "tool", Name: "tool_search", Content: searchResult("current_unit_tool")},
	}
	ctx := withToolActivationState(context.Background())
	activateDeferredTools(ctx, []string{"stale_in_memory"})
	if restored := seedToolActivationFromMessages(ctx, replayed); restored != 1 {
		t.Fatalf("restored=%d, want only the current unit's activation", restored)
	}
	if deferredToolActive(ctx, "old_unit_tool") || deferredToolActive(ctx, "stale_in_memory") {
		t.Fatal("activation from an older unit survived replay")
	}
	if !deferredToolActive(ctx, "current_unit_tool") {
		t.Fatal("current work-unit activation was not restored")
	}
}

func TestUpdatePlanIsolatesMixedToolCallBatch(t *testing.T) {
	calls := []llm.ToolCall{
		{ID: "read", Function: "read_file"},
		{ID: "plan", Function: "update_plan"},
		{ID: "write", Function: "apply_patch"},
	}
	got, deferred := isolateWorkUnitBoundaryCall(calls)
	if len(got) != 1 || got[0].ID != "plan" || deferred != 2 {
		t.Fatalf("isolated=%+v deferred=%d", got, deferred)
	}
	plain := []llm.ToolCall{{ID: "read", Function: "read_file"}, {ID: "search", Function: "search_files"}}
	got, deferred = isolateWorkUnitBoundaryCall(plain)
	if len(got) != len(plain) || deferred != 0 {
		t.Fatalf("ordinary batch changed: %+v deferred=%d", got, deferred)
	}
}

func TestInProgressWorkUnitSequenceIgnoresOtherTools(t *testing.T) {
	if got := inProgressWorkUnitSequence("tool_search", planProjection(2)); got != 0 {
		t.Errorf("only update_plan projections carry a work unit, got %d", got)
	}
	if got := inProgressWorkUnitSequence("update_plan", `{"work_units":[{"sequence":5,"plan_status":"completed"}]}`); got != 0 {
		t.Errorf("a plan with no in-progress unit has no sequence, got %d", got)
	}
	if got := inProgressWorkUnitSequence("update_plan", "not json"); got != 0 {
		t.Errorf("unparsable content must not report a sequence, got %d", got)
	}
}
