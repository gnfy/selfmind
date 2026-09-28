package cli

import (
	"fmt"
	"strings"
	"testing"
)

// The pinned plan says how many actions ago it last moved, so a plan that has
// sat on its first step through a long run reads as possibly stale instead of
// as the current state; a new snapshot resets it.
func TestPinnedPlanSaysHowLongAgoItMoved(t *testing.T) {
	m, _ := newApprovalTestModel()
	m.Update(MsgDaemonRunStarted{RunID: "run_a", Input: "release", Event: sessionRef("run_a", "ev0", "", false)})
	plan := `{"plan_version":%d,"plan":[{"step":"publish","status":"in_progress"},{"step":"announce","status":"pending"}]}`
	m.Update(MsgPlanUpdated{Content: fmt.Sprintf(plan, 1), Event: sessionRef("run_a", "ev1", "", true)})
	for i := 1; i <= 3; i++ {
		m.Update(MsgToolStart{ToolName: "terminal", ToolCallID: fmt.Sprintf("call_%d", i), Event: sessionRef("run_a", fmt.Sprintf("ev_tool_%d", i), "", true)})
	}
	if block := stripANSI(m.activePlanBlock(100)); !strings.Contains(block, "Plan · 0/2 · updated 3 actions ago") {
		t.Fatalf("pinned plan does not say how long ago it moved:\n%s", block)
	}
	m.Update(MsgPlanUpdated{Content: fmt.Sprintf(plan, 2), Event: sessionRef("run_a", "ev2", "", true)})
	if block := stripANSI(m.activePlanBlock(100)); strings.Contains(block, "updated") {
		t.Fatalf("a fresh snapshot still reads as stale:\n%s", block)
	}
}
