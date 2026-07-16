package cli

import (
	"strings"
	"testing"
)

func TestLivePlanReplacesSnapshotAboveComposer(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
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
	if strings.Count(rendered, "Updated plan") != 1 {
		t.Fatalf("expected exactly one live plan, got: %q", rendered)
	}
	for _, step := range []string{"inspect files", "apply changes", "run tests"} {
		if !strings.Contains(rendered, step) {
			t.Fatalf("latest plan missing %q: %q", step, rendered)
		}
	}
	planAt := strings.Index(rendered, "Updated plan")
	composerAt := strings.Index(rendered, "Ask SelfMind")
	if planAt < 0 || composerAt < 0 || planAt >= composerAt {
		t.Fatalf("live plan must render above the composer: %q", rendered)
	}
	block := model.activePlanBlock(model.width)
	if !strings.HasPrefix(block, "\n") || !strings.HasSuffix(block, "\n") {
		t.Fatalf("live plan must keep one-row breathing room above and below: %q", block)
	}
}

func TestLivePlanClearsWhenRunFinishes(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.width = 100
	model.height = 30
	model.runStatus = "working"
	model.activePlanJSON = `{"plan":[{"step":"finish","status":"completed"}]}`

	updated, _ := model.Update(MsgAgentDone{Response: "Done."})
	model = updated.(*uiModel)
	if model.activePlanJSON != "" {
		t.Fatalf("active plan was not cleared: %q", model.activePlanJSON)
	}
	if rendered := stripANSI(model.viewActiveRegion()); strings.Contains(rendered, "Updated plan") {
		t.Fatalf("finished UI must not retain the live plan: %q", rendered)
	}
}
