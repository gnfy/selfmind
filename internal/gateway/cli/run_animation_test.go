package cli

import (
	"testing"

	"selfmind/internal/gateway/api"
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
