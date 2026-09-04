package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
)

// TestVisibleProgressRowIsAlwaysTicking is the invariant behind the reported
// "Working (37s)" that sat frozen over a finished run. The row's visibility and
// the tick that repaints it were two different predicates, so any state covered
// by one but not the other showed a row nothing was driving — it froze at
// whatever second the chain happened to die. Whenever the row is visible, both
// chains must re-arm.
func TestVisibleProgressRowIsAlwaysTicking(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(m *uiModel)
	}{
		{"model wait", func(m *uiModel) { m.waitingForModel = true }},
		{"thinking", func(m *uiModel) { m.thinking = true }},
		{"tool running", func(m *uiModel) { m.toolExecuting = "run-ci.sh" }},
		{"local request", func(m *uiModel) { m.localRequestActive = true }},
		{"owned daemon run", func(m *uiModel) { m.daemonRunActive = true; m.daemonRunOwned = true }},
	} {
		model := NewController("", "", nil, "").model
		model.width = 100
		model.thinkingStart = time.Now().Add(-37 * time.Second)
		tc.apply(model)

		row := stripANSI(model.activityRow(100))
		if strings.TrimSpace(row) == "" {
			t.Fatalf("%s: expected a visible progress row", tc.name)
		}
		if _, cmd := model.Update(MsgWorkingTick{}); cmd == nil {
			t.Fatalf("%s: row is visible but the elapsed tick stopped — it would freeze", tc.name)
		}
		if !model.spinnerRunning {
			t.Fatalf("%s: row is visible but no animation chain owns it", tc.name)
		}
		if _, cmd := model.Update(spinner.TickMsg{}); cmd == nil {
			t.Fatalf("%s: row is visible but the animation stopped", tc.name)
		}
	}
}

// TestIdleTurnDrivesNothing is the other half: an idle session must not keep
// waking up, at 10 FPS or at all.
func TestIdleTurnDrivesNothing(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.width = 100
	if model.animatingTurn() {
		t.Fatal("a fresh controller should not be animating")
	}
	if row := stripANSI(model.activityRow(100)); strings.TrimSpace(row) != "" {
		t.Fatalf("idle should render no progress row, got %q", row)
	}
	if _, cmd := model.Update(MsgWorkingTick{}); cmd != nil {
		t.Fatal("an idle working tick should not re-arm")
	}
	if _, cmd := model.Update(spinner.TickMsg{}); cmd != nil {
		t.Fatal("an idle spinner tick should not re-arm")
	}
}
