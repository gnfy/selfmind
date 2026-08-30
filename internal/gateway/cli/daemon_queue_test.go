package cli

import (
	"strings"
	"testing"
	"time"

	"selfmind/internal/gateway/api"

	tea "github.com/charmbracelet/bubbletea"
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

	updated, cmd := m.Update(MsgDaemonRunStarted{
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
	if line := m.statusLine(); !strings.Contains(line, "background watcher finalizing") || strings.Contains(line, "working ") {
		t.Fatalf("background finalization status = %q", line)
	}
	updated, cmd = m.Update(MsgWorkingTick(time.Now()))
	m = updated.(*uiModel)
	if cmd != nil {
		t.Fatal("background watcher finalization rescheduled the foreground working clock")
	}
}

// A watcher finalization run executes in the daemon on the person's behalf.
// Moving the wait off the agent turn is pointless if the terminal then
// replays the whole background transcript, so only its start notice and its
// recorded outcome may reach this terminal.
func TestWatcherFinalizationRunRendersResultNotProcess(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	const runID = "run_finalize"
	ref := func(eventID string) uiEventRef {
		return uiEventRef{Source: eventSourceDaemon, RunID: runID, EventID: eventID}
	}

	updated, _ := model.Update(MsgDaemonRunStarted{
		RunID:      runID,
		WatchID:    "watch_123",
		Origin:     "watch",
		TaskStatus: "running",
		Started:    time.Now(),
		Event:      ref("evt_start"),
	})
	m := updated.(*uiModel)
	afterStart := len(m.messages)

	for _, msg := range []tea.Msg{
		MsgAgentActivity{Content: "Reading the release record", Event: ref("evt_activity")},
		MsgStream{Content: "Backfilling the release record now.", Event: ref("evt_delta")},
		MsgToolStart{ToolName: "read_file", ToolCallID: "call_1", Event: ref("evt_tool_start")},
		MsgToolOutput{ToolName: "read_file", ToolCallID: "call_1", Content: "file body", Event: ref("evt_tool_output")},
		MsgToolHeartbeat{ToolName: "read_file", ToolCallID: "call_1", Event: ref("evt_tool_beat")},
		MsgToolDone{ToolName: "read_file", ToolCallID: "call_1", Result: "ok", Event: ref("evt_tool_done")},
		MsgPlanUpdated{Content: `{"steps":[{"title":"backfill"}]}`, Event: ref("evt_plan")},
		MsgLearningEvent{Content: "Learning: release records need a status field", Event: ref("evt_learning")},
	} {
		updated, _ = m.Update(msg)
		m = updated.(*uiModel)
	}

	if len(m.messages) != afterStart {
		t.Fatalf("background run added %d transcript cells: %+v", len(m.messages)-afterStart, m.messages[afterStart:])
	}
	if m.processState().HasStreamContent() || m.activePlanJSON != "" || m.activityText != "" || m.statusMsg != "" {
		t.Fatalf("background run leaked live state: stream=%q plan=%q activity=%q status=%q",
			m.processState().previewContent(), m.activePlanJSON, m.activityText, m.statusMsg)
	}

	updated, _ = m.Update(MsgDaemonRunFinished{
		RunID:   runID,
		Status:  "waiting_user",
		Summary: "The check environment was blocked; the external state was not observed.",
		Event:   ref("evt_finished"),
	})
	m = updated.(*uiModel)
	last := m.messages[len(m.messages)-1]
	if last.Role != "notice" {
		t.Fatalf("finalization result role = %q", last.Role)
	}
	if !strings.HasPrefix(last.Content, "Watcher watch_123 | status: finalized | task: waiting_user") {
		t.Fatalf("finalization result = %q", last.Content)
	}
	if !strings.Contains(last.Content, "the external state was not observed") {
		t.Fatalf("finalization result dropped the summary: %q", last.Content)
	}
	if len(m.messages) != afterStart+1 {
		t.Fatalf("finish added %d cells, want 1", len(m.messages)-afterStart)
	}

	// A trailing event from that run must not surface after it ended.
	updated, _ = m.Update(MsgStream{Content: "late delta", Event: ref("evt_late")})
	m = updated.(*uiModel)
	if m.processState().HasStreamContent() || len(m.messages) != afterStart+1 {
		t.Fatalf("trailing background event rendered: stream=%q cells=%d", m.processState().previewContent(), len(m.messages))
	}
}

// The rule is the run's origin, not the watcher: any run the daemon starts on
// the person's behalf reports its result instead of its process. A cron fire
// has no boundary the person already saw, so it stays silent until it has
// something to report.
func TestCronRunRendersResultWithoutStartNoticeOrProcess(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	const runID = "run_cron"
	ref := func(eventID string) uiEventRef {
		return uiEventRef{Source: eventSourceDaemon, RunID: runID, EventID: eventID}
	}

	updated, _ := model.Update(MsgDaemonRunStarted{
		RunID:   runID,
		Origin:  "cron",
		Input:   "summarize yesterday's builds",
		Started: time.Now(),
		Event:   ref("evt_start"),
	})
	m := updated.(*uiModel)
	if len(m.messages) != 0 {
		t.Fatalf("cron start wrote %d cells: %+v", len(m.messages), m.messages)
	}
	if !m.daemonRunActive || m.runStatus != "working" {
		t.Fatalf("cron run must stay visible as daemon work: active=%v status=%q", m.daemonRunActive, m.runStatus)
	}

	for _, msg := range []tea.Msg{
		MsgStream{Content: "Three builds succeeded.", Event: ref("evt_delta")},
		MsgToolStart{ToolName: "terminal", ToolCallID: "call_1", Event: ref("evt_tool_start")},
		MsgToolDone{ToolName: "terminal", ToolCallID: "call_1", Result: "ok", Event: ref("evt_tool_done")},
	} {
		updated, _ = m.Update(msg)
		m = updated.(*uiModel)
	}
	if len(m.messages) != 0 || m.processState().HasStreamContent() {
		t.Fatalf("cron progress leaked: cells=%d stream=%q", len(m.messages), m.processState().previewContent())
	}

	updated, _ = m.Update(MsgDaemonRunFinished{
		RunID:   runID,
		Status:  "done",
		Summary: "Three builds succeeded overnight.",
		Event:   ref("evt_finished"),
	})
	m = updated.(*uiModel)
	if len(m.messages) != 1 {
		t.Fatalf("cron finish wrote %d cells, want 1", len(m.messages))
	}
	last := m.messages[0]
	if last.Role != "notice" || !strings.HasPrefix(last.Content, "Background run (cron) | task: done") {
		t.Fatalf("cron result = role %q content %q", last.Role, last.Content)
	}
	if !strings.Contains(last.Content, "Three builds succeeded overnight.") {
		t.Fatalf("cron result dropped the summary: %q", last.Content)
	}
}

// The suppression is scoped to the background run id: an ordinary daemon run
// that starts afterwards still streams its progress to this terminal.
func TestForegroundRunStillStreamsAfterBackgroundRun(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.markBackgroundRun("run_finalize", "watch_123", "watch")

	updated, _ := model.Update(MsgDaemonRunStarted{
		RunID:   "run_user",
		Input:   "fix the failing test",
		Started: time.Now(),
		Event:   uiEventRef{Source: eventSourceDaemon, RunID: "run_user", EventID: "evt_user_start"},
	})
	m := updated.(*uiModel)
	updated, _ = m.Update(MsgToolStart{
		ToolName:   "read_file",
		ToolCallID: "call_user",
		Event:      uiEventRef{Source: eventSourceDaemon, RunID: "run_user", EventID: "evt_user_tool"},
	})
	m = updated.(*uiModel)
	tools := m.processState().tools
	if len(tools) != 1 || tools[0].Role != "tool" || tools[0].ToolName != "read_file" {
		t.Fatalf("foreground tool cell missing: %+v", tools)
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
