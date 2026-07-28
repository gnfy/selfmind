package cli

import (
	"strings"
	"testing"
	"time"

	"selfmind/internal/gateway/api"
)

func TestQueuedTurnTransitionsThroughDaemonLifecycle(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	input := "check the release and merge the PR"

	model.localRequestActive = true
	model.localRequestInput = input
	updated, _ := model.Update(MsgAgentDone{
		Response: "Queued behind the running task.",
		Input:    input,
		Turn:     &api.TurnStatus{Status: "queued"},
	})
	m := updated.(*uiModel)
	if m.runStatus != "queued" || m.queuedCount != 1 || m.localRequestActive {
		t.Fatalf("queued state = status %q count %d local %v", m.runStatus, m.queuedCount, m.localRequestActive)
	}

	started := time.Now().Add(-2 * time.Second)
	updated, _ = m.Update(MsgDaemonRunStarted{
		RunID:   "run_queued",
		Input:   input,
		Started: started,
		Event:   uiEventRef{Source: eventSourceDaemon, RunID: "run_queued", EventID: "evt_started"},
	})
	m = updated.(*uiModel)
	if !m.daemonRunActive || m.daemonRunID != "run_queued" || m.runStatus != "working" {
		t.Fatalf("started state = active %v id %q status %q", m.daemonRunActive, m.daemonRunID, m.runStatus)
	}
	if m.queuedCount != 0 || m.localRequestActive {
		t.Fatalf("started queued run should be observed, not own input: count=%d local=%v", m.queuedCount, m.localRequestActive)
	}
	if got := m.messages[len(m.messages)-1].Content; !strings.Contains(got, "Queued task started") {
		t.Fatalf("missing queued start notice: %q", got)
	}

	updated, _ = m.Update(MsgStream{
		Content: "Detailed final answer.",
		Event:   uiEventRef{Source: eventSourceDaemon, RunID: "run_queued", EventID: "evt_delta"},
	})
	m = updated.(*uiModel)
	updated, _ = m.Update(MsgDaemonRunFinished{
		RunID:   "run_queued",
		Status:  "done",
		Summary: "Short summary.",
		Event:   uiEventRef{Source: eventSourceDaemon, RunID: "run_queued", EventID: "evt_finished"},
	})
	m = updated.(*uiModel)
	if m.daemonRunActive || m.runStatus != "done" {
		t.Fatalf("finished state = active %v status %q", m.daemonRunActive, m.runStatus)
	}
	last := m.messages[len(m.messages)-1]
	if last.Role != "assistant" || last.Content != "Detailed final answer." {
		t.Fatalf("final answer = role %q content %q", last.Role, last.Content)
	}
}

func TestDaemonFinishWithoutObservedStartDoesNotChangeStatus(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.runStatus = "ready"

	updated, _ := model.Update(MsgDaemonRunFinished{
		RunID:  "run_stale",
		Status: "done",
		Event:  uiEventRef{Source: eventSourceDaemon, RunID: "run_stale", EventID: "evt_stale"},
	})
	m := updated.(*uiModel)
	if m.runStatus != "ready" {
		t.Fatalf("stale finish changed status to %q", m.runStatus)
	}
}

func TestWatcherLifecycleUsesCompactIDNotices(t *testing.T) {
	model := NewController(nil, nil, nil, "").model

	updated, _ := model.Update(MsgWatcherCompleted{
		WatchID:    "watch_123",
		Status:     "succeeded",
		TaskStatus: "waiting_finalization",
		Event:      uiEventRef{Source: eventSourceDaemon, RunID: "run_origin", EventID: "evt_watch_done"},
	})
	m := updated.(*uiModel)
	if got := m.messages[len(m.messages)-1].Content; got != "Watcher watch_123 | status: succeeded | task: waiting_finalization" {
		t.Fatalf("completion notice = %q", got)
	}

	updated, _ = m.Update(MsgDaemonRunStarted{
		RunID:      "run_finalize",
		WatchID:    "watch_123",
		TaskStatus: "running",
		Input:      "internal finalization prompt that must not be rendered",
		Started:    time.Now(),
		Event:      uiEventRef{Source: eventSourceDaemon, RunID: "run_finalize", EventID: "evt_finalize_start"},
	})
	m = updated.(*uiModel)
	if got := m.messages[len(m.messages)-1].Content; got != "Watcher watch_123 | status: finalizing | task: running" {
		t.Fatalf("finalization notice = %q", got)
	}
	if strings.Contains(m.messages[len(m.messages)-1].Content, "internal finalization prompt") {
		t.Fatalf("internal finalization prompt leaked into notice")
	}
}

func TestOlderSynchronousReplyDoesNotClearNewerDaemonRun(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.localRequestActive = true
	model.localRequestInput = "first"
	model.daemonRunActive = true
	model.daemonRunID = "run_newer"
	model.daemonRunAwaitingDone = false
	model.runStatus = "working"

	updated, _ := model.Update(MsgAgentDone{
		Response: "first completed",
		Turn:     &api.TurnStatus{RunID: "run_older", Status: "done"},
	})
	m := updated.(*uiModel)
	if !m.daemonRunActive || m.daemonRunID != "run_newer" || m.runStatus != "working" {
		t.Fatalf("newer run was cleared: active=%v id=%q status=%q", m.daemonRunActive, m.daemonRunID, m.runStatus)
	}
}
