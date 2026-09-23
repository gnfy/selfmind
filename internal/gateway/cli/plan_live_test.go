package cli

import (
	"strings"
	"testing"
)

func TestLivePlanReplacesSnapshotAboveComposer(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.width = 100
	model.height = 30
	model.runStatus = "working"

	first := `{"plan":[{"step":"inspect files","status":"in_progress"},{"step":"run tests","status":"pending"}]}`
	updated, _ := model.Update(MsgPlanUpdated{Content: first})
	model = updated.(*uiModel)

	second := `{"plan":[{"step":"inspect files","status":"completed"},{"step":"apply changes","status":"in_progress"},{"step":"run tests","status":"pending"}]}`
	updated, _ = model.Update(MsgPlanUpdated{Content: second})
	model = updated.(*uiModel)

	if len(model.messages) != 0 {
		t.Fatalf("plan snapshots must not create transcript messages: %+v", model.messages)
	}
	rendered := stripANSI(model.viewActiveRegion())
	if strings.Count(rendered, "Plan ·") != 1 {
		t.Fatalf("expected exactly one live plan, got: %q", rendered)
	}
	for _, step := range []string{"inspect files", "apply changes", "run tests"} {
		if !strings.Contains(rendered, step) {
			t.Fatalf("latest plan missing %q: %q", step, rendered)
		}
	}
	planAt := strings.Index(rendered, "Plan ·")
	composerAt := strings.Index(rendered, "Ask SelfMind")
	if planAt < 0 || composerAt < 0 || planAt >= composerAt {
		t.Fatalf("live plan must render above the composer: %q", rendered)
	}
	block := model.activePlanBlock(model.width)
	if !strings.HasPrefix(block, "\n") || !strings.HasSuffix(block, "\n") {
		t.Fatalf("live plan must keep one-row breathing room above and below: %q", block)
	}
}

func TestLivePlanRejectsStaleVersionAndForeignRun(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.runStatus = "working"
	current := `{"plan_version":2,"plan":[{"step":"apply","status":"in_progress"}]}`
	model.Update(MsgPlanUpdated{Content: current, Event: uiEventRef{RunID: "run-current", Cursor: 20}})

	stale := `{"plan_version":1,"plan":[{"step":"inspect","status":"in_progress"}]}`
	model.Update(MsgPlanUpdated{Content: stale, Event: uiEventRef{RunID: "run-current", Cursor: 21}})
	if model.activePlanJSON != current || model.activePlanVersion != 2 {
		t.Fatalf("stale version replaced canonical plan: %q v%d", model.activePlanJSON, model.activePlanVersion)
	}

	foreign := `{"plan_version":3,"plan":[{"step":"old run","status":"in_progress"}]}`
	model.Update(MsgPlanUpdated{Content: foreign, Event: uiEventRef{RunID: "run-old", Cursor: 30}})
	if model.activePlanJSON != current || model.activePlanRunID != "run-current" {
		t.Fatalf("foreign run replaced canonical plan: %q run=%q", model.activePlanJSON, model.activePlanRunID)
	}

	next := `{"plan_version":3,"plan":[{"step":"verify","status":"in_progress"}]}`
	model.Update(MsgPlanUpdated{Content: next, Event: uiEventRef{RunID: "run-current", Cursor: 22}})
	if model.activePlanJSON != next || model.activePlanVersion != 3 {
		t.Fatalf("newer plan was not projected: %q v%d", model.activePlanJSON, model.activePlanVersion)
	}
}

func TestClearActivePlanResetsProjectionOwnership(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.applyPlanSnapshot(`{"plan_version":4,"plan":[{"step":"done","status":"completed"}]}`, uiEventRef{RunID: "run-one", Cursor: 9})
	model.clearActivePlan()
	if model.activePlanJSON != "" || model.activePlanRunID != "" || model.activePlanVersion != 0 || model.activePlanCursor != 0 {
		t.Fatalf("plan projection was not fully cleared: %+v", model)
	}
	if !model.applyPlanSnapshot(`{"plan_version":1,"plan":[{"step":"new","status":"in_progress"}]}`, uiEventRef{RunID: "run-two", Cursor: 1}) {
		t.Fatal("new run could not claim cleared plan projection")
	}
}

func TestLivePlanClearsWhenRunFinishes(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.width = 100
	model.height = 30
	model.runStatus = "working"
	model.activePlanJSON = `{"plan":[{"step":"finish","status":"completed"}]}`

	updated, _ := model.Update(MsgAgentDone{Response: "Done."})
	model = updated.(*uiModel)
	if model.activePlanJSON != "" {
		t.Fatalf("active plan was not cleared: %q", model.activePlanJSON)
	}
	if rendered := stripANSI(model.viewActiveRegion()); strings.Contains(rendered, "Plan ·") {
		t.Fatalf("finished UI must not retain the live plan: %q", rendered)
	}
}

func TestWaitingAnimationKeepsOneRowWhenPlanConsumesTightLayout(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.width = 48
	model.height = 6
	model.runStatus = "working"
	model.activePlanJSON = `{"plan":[{"step":"inspect files","status":"completed"},{"step":"apply changes","status":"in_progress"},{"step":"run tests","status":"pending"}]}`
	model.startModelWait("Reading tool results and deciding the next step")

	// The progress line has its own reserved row, so a tight layout can starve
	// the tool/process rows without ever costing the animation its slot.
	// The label is truncated to this narrow width; its presence is the point.
	if rendered := stripANSI(model.activityRow(model.width)); !strings.Contains(rendered, "Reading tool results") {
		t.Fatalf("waiting animation disappeared beside a live plan: %q", rendered)
	}
}
