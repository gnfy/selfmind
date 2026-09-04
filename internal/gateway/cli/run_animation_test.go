package cli

import (
	"strings"
	"testing"

	"github.com/muesli/termenv"

	"selfmind/internal/gateway/api"
	"selfmind/internal/ui/common"
	uitheme "selfmind/internal/ui/theme"
)

// A message typed while a daemon-owned run is active is steered into that
// run; the daemon acknowledges with Turn.Status "accepted". That ack is not
// the run's terminal answer, so it must not finalize tool cells, clear the
// plan, or mark the run done (observed live: the transcript went "done" and
// dropped every later tool event while the run kept executing).
func TestAcceptedSteerAckKeepsActiveRunState(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.daemonRunActive = true
	model.daemonRunID = "run_active"
	model.daemonRunOwned = true
	model.localRequestActive = true
	model.localRequestInput = "use pnpm instead"
	model.runStatus = "working"
	model.toolExecuting = "terminal"
	model.activePlanJSON = `{"plan":[{"step":"install deps","status":"in_progress"}]}`

	updated, _ := model.Update(MsgAgentDone{
		Response: "Added your guidance to the running task.",
		Input:    "use pnpm instead",
		Turn:     &api.TurnStatus{Status: "accepted", TaskStatus: "running", BackgroundStatus: "running", RunID: "run_active"},
	})
	m := updated.(*uiModel)
	if m.runStatus != "working" {
		t.Fatalf("steer ack marked the live run %q", m.runStatus)
	}
	if !m.daemonRunActive || m.daemonRunID != "run_active" {
		t.Fatalf("steer ack cleared the daemon run: active=%v id=%q", m.daemonRunActive, m.daemonRunID)
	}
	if m.toolExecuting != "terminal" || m.activePlanJSON == "" {
		t.Fatalf("steer ack tore down live state: tool=%q plan=%q", m.toolExecuting, m.activePlanJSON)
	}
	if m.localRequestActive {
		t.Fatal("the steered local request must be released so the next input routes as guidance again")
	}
	for _, message := range m.messages {
		if message.Role == "assistant" && message.Content == "Added your guidance to the running task." {
			t.Fatal("steer ack must not enter the transcript as a final answer")
		}
	}
}

// A queued message the person typed here drains as a daemon run. Its
// activity events must animate exactly like a local turn; treating them as
// passive left the spinner dark for every queued turn.
func TestQueuedRunActivityAnimatesSpinner(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.queuedInputs = []string{"fix the failing test"}
	model.queuedCount = 1

	updated, _ := model.Update(MsgDaemonRunStarted{
		RunID: "run_queued", Input: "fix the failing test",
		Event: uiEventRef{Source: eventSourceDaemon, RunID: "run_queued", EventID: "evt_start"},
	})
	m := updated.(*uiModel)
	if !m.daemonRunOwned {
		t.Fatal("a drained queued run must be owned by the terminal that queued it")
	}
	if m.daemonRunAwaitingDone {
		t.Fatal("a queued run has no synchronous reply; it must not wait for MsgAgentDone")
	}
	updated, cmd := m.Update(MsgAgentActivity{
		Content: "Waiting for the model", Phase: modelThinkingPhase,
		Event: uiEventRef{Source: eventSourceDaemon, RunID: "run_queued", EventID: "evt_think"},
	})
	m = updated.(*uiModel)
	if !m.waitingForModel || cmd == nil {
		t.Fatalf("queued run activity did not start the waiting animation: waiting=%v cmd=%v", m.waitingForModel, cmd != nil)
	}
}

func TestTransferredContinuationAnimatesByExactQueueID(t *testing.T) {
	model := NewController("", "", nil, "").model
	updated, _ := model.Update(MsgAgentDone{
		Response: "Historical work was queued as the next exact continuation.",
		Input:    "确认执行",
		Turn:     &api.TurnStatus{Status: "done", QueueID: "queue-transfer"},
	})
	m := updated.(*uiModel)
	updated, _ = m.Update(MsgDaemonRunStarted{
		RunID: "run-child", QueueID: "queue-transfer", Input: "确认执行",
		Event: uiEventRef{Source: eventSourceDaemon, RunID: "run-child", EventID: "evt-child-start"},
	})
	m = updated.(*uiModel)
	if !m.daemonRunOwned {
		t.Fatal("the exact continuation queued by this terminal must own its animation")
	}
	updated, cmd := m.Update(MsgAgentActivity{
		Content: "Waiting for the model", Phase: modelThinkingPhase,
		Event: uiEventRef{Source: eventSourceDaemon, RunID: "run-child", EventID: "evt-child-think"},
	})
	m = updated.(*uiModel)
	if !m.waitingForModel || cmd == nil {
		t.Fatalf("transferred continuation did not animate: waiting=%v cmd=%v", m.waitingForModel, cmd != nil)
	}
}

func TestContinuationQueueAckClaimsAlreadyStartedRun(t *testing.T) {
	model := NewController("", "", nil, "").model
	updated, _ := model.Update(MsgDaemonRunStarted{
		RunID: "run-child", QueueID: "queue-race", Input: "确认执行",
		Event: uiEventRef{Source: eventSourceDaemon, RunID: "run-child", EventID: "evt-start-first"},
	})
	m := updated.(*uiModel)
	if m.daemonRunOwned {
		t.Fatal("the run cannot be owned before its exact acknowledgement arrives")
	}
	updated, _ = m.Update(MsgAgentDone{
		Response: "Historical work was queued as the next exact continuation.",
		Input:    "确认执行",
		Turn:     &api.TurnStatus{Status: "done", RunID: "run-interaction", QueueID: "queue-race"},
	})
	m = updated.(*uiModel)
	if !m.daemonRunOwned || m.daemonRunID != "run-child" || m.runStatus != "working" {
		t.Fatalf("late queue acknowledgement did not claim the active child: owned=%v run=%q status=%q", m.daemonRunOwned, m.daemonRunID, m.runStatus)
	}
}

func TestEqualQueuedTextWithDifferentQueueIDStaysPassive(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.queuedRunIDs = []string{"queue-local"}
	model.queuedCount = 1
	updated, _ := model.Update(MsgDaemonRunStarted{
		RunID: "run-foreign", QueueID: "queue-foreign", Input: "确认执行",
		Event: uiEventRef{Source: eventSourceDaemon, RunID: "run-foreign", EventID: "evt-foreign"},
	})
	m := updated.(*uiModel)
	if m.daemonRunOwned || m.queuedCount != 1 {
		t.Fatalf("equal text overrode exact queue identity: owned=%v queued=%d", m.daemonRunOwned, m.queuedCount)
	}
}

// Events from a run this terminal did not submit stay passive: no spinner, the
// status bar alone reports the daemon is busy.
func TestForeignDaemonRunActivityStaysPassive(t *testing.T) {
	model := NewController("", "", nil, "").model
	updated, _ := model.Update(MsgDaemonRunStarted{
		RunID: "run_other", Input: "work started from another endpoint",
		Event: uiEventRef{Source: eventSourceDaemon, RunID: "run_other", EventID: "evt_start_other"},
	})
	m := updated.(*uiModel)
	updated, _ = m.Update(MsgAgentActivity{
		Content: "Waiting for the model", Phase: modelThinkingPhase,
		Event: uiEventRef{Source: eventSourceDaemon, RunID: "run_other", EventID: "evt_think_other"},
	})
	m = updated.(*uiModel)
	if m.waitingForModel {
		t.Fatal("a foreign daemon run must not animate this terminal's spinner")
	}
}

// The progress line must survive every live phase, not only a provider wait.
// Live symptom (2026-09-03): the animation vanished while a tool ran and while
// a tall plan consumed the process-row budget, because the spinner shared that
// budget at the top of the active region.
func TestActivityRowSurvivesToolExecutionAndTightLayout(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.width = 80
	model.height = 20
	model.runStatus = "working"

	// A running tool is progress even though no model wait is outstanding.
	model.toolExecuting = "terminal"
	if row := stripANSI(model.activityRow(model.width)); !strings.Contains(row, "Running terminal") {
		t.Fatalf("a running tool must keep the progress row: %q", row)
	}
	model.toolExecuting = ""
	if row := stripANSI(model.activityRow(model.width)); row != "" {
		t.Fatalf("an idle terminal must not animate: %q", row)
	}

	// A tight layout may starve the tool/process rows; the progress row keeps
	// its own reserved slot.
	model.height = 6
	model.activePlanJSON = `{"plan":[{"step":"inspect","status":"completed"},{"step":"apply","status":"in_progress"},{"step":"verify","status":"pending"}]}`
	model.startModelWait("Waiting for the model to decide after tool results")
	if budget := model.processRowBudget(model.width); budget != 0 {
		t.Logf("process budget in a tight layout = %d", budget)
	}
	if row := stripANSI(model.activityRow(model.width)); !strings.Contains(row, "Waiting for the model") {
		t.Fatalf("tight layout lost the progress row: %q", row)
	}
}

// The row's position is the fix: below the plan, above the composer. The plan
// keeps the position it already had, so a growing plan cannot move the
// animation and a growing process surface cannot push it off screen.
func TestActivityRowSitsBelowPlanAndAboveComposer(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.width = 80
	model.height = 30
	model.runStatus = "working"
	model.activePlanJSON = `{"plan":[{"step":"inspect files","status":"in_progress"}]}`
	model.startModelWait("Waiting for the model to choose the first step")

	rendered := stripANSI(model.viewActiveRegion())
	planAt := strings.Index(rendered, "inspect files")
	activityAt := strings.Index(rendered, "Waiting for the model to choose the first step")
	if planAt < 0 || activityAt < 0 {
		t.Fatalf("plan and progress row must both render:\n%s", rendered)
	}
	if planAt > activityAt {
		t.Fatalf("the plan must stay above the progress row:\n%s", rendered)
	}
	// Everything after the progress row belongs to the composer band.
	if statusAt := strings.LastIndex(rendered, "\n"); statusAt >= 0 && statusAt < activityAt {
		t.Fatalf("the progress row must sit above the composer band:\n%s", rendered)
	}
}

// The progress row is its own band: one blank line above and one below, so it
// never reads as part of the Composer frame. Live symptom (2026-09-03): the
// plan kept its gap while the row sat flush against the Message box.
func TestActivityRowKeepsSymmetricSpacingAroundItself(t *testing.T) {
	withPlan := NewController("", "", nil, "").model
	withPlan.width = 80
	withPlan.height = 30
	withPlan.runStatus = "working"
	withPlan.activePlanJSON = `{"plan":[{"step":"inspect files","status":"in_progress"}]}`
	withPlan.startModelWait("Waiting for the model to choose the first step")

	// The plan band already ends with a blank row, so the block adds only the
	// trailing one; the generic composer spacer must not double it.
	block := withPlan.activityRowBlock(withPlan.width)
	if !strings.HasSuffix(block, "\n") || strings.HasPrefix(block, "\n") {
		t.Fatalf("with a plan above, the block owns only its trailing blank: %q", block)
	}
	if gap := withPlan.composerGapHeight(); gap != 0 {
		t.Fatalf("composer spacer must not double the row's own blank: gap=%d", gap)
	}
	rendered := stripANSI(withPlan.viewActiveRegion())
	lines := strings.Split(rendered, "\n")
	rowAt := -1
	for i, line := range lines {
		if strings.Contains(line, "Waiting for the model to choose the first step") {
			rowAt = i
			break
		}
	}
	if rowAt <= 0 || rowAt+1 >= len(lines) {
		t.Fatalf("progress row not found with room on both sides:\n%s", rendered)
	}
	if strings.TrimSpace(lines[rowAt-1]) != "" || strings.TrimSpace(lines[rowAt+1]) != "" {
		t.Fatalf("progress row must have one blank line on each side:\nabove=%q\nrow=%q\nbelow=%q",
			lines[rowAt-1], lines[rowAt], lines[rowAt+1])
	}

	// Without a plan nothing above supplies the blank, so the block adds both.
	noPlan := NewController("", "", nil, "").model
	noPlan.width = 80
	noPlan.height = 30
	noPlan.runStatus = "working"
	noPlan.startModelWait("Waiting for the model")
	bare := noPlan.activityRowBlock(noPlan.width)
	if !strings.HasPrefix(bare, "\n") || !strings.HasSuffix(bare, "\n") {
		t.Fatalf("without a plan the block owns both blanks: %q", bare)
	}
}

// The row is status, not evidence: its glyph takes the accent role and its
// label the mainline foreground, so it is not the same color as tool results.
func TestActivityRowUsesProgressStylesNotToolEvidence(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.width = 80
	model.height = 20
	model.runStatus = "working"
	model.startModelWait("Waiting for the model")

	if row := model.activityRow(model.width); row == "" {
		t.Fatal("progress row did not render")
	}
	// Assert the palette, not the rendered bytes: a test process has no color
	// profile, so every style renders as identical plain text and the resolved
	// theme collapses its roles. Resolve a true-color theme explicitly so the
	// role assignment itself is what is under test.
	colored, err := uitheme.Resolve(uitheme.Options{Mode: uitheme.ModeDark, Profile: termenv.TrueColor, DarkBackground: true})
	if err != nil {
		t.Fatal(err)
	}
	chat := common.StylesFor(colored).Chat
	if chat.ProgressLabel.GetForeground() == chat.ToolResult.GetForeground() {
		t.Fatal("the progress label must not share tool evidence's color")
	}
	if chat.ProgressLabel.GetForeground() != chat.UserText.GetForeground() {
		t.Fatal("the progress label uses the mainline foreground")
	}
	if chat.ProgressGlyph.GetForeground() != chat.ToolName.GetForeground() {
		t.Fatal("the moving glyph uses the accent role, like a semantic action")
	}
	if chat.ProgressGlyph.GetForeground() == chat.ProgressLabel.GetForeground() {
		t.Fatal("only the glyph takes the accent; the label must stay mainline")
	}
}
