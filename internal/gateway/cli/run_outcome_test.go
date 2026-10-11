package cli

import (
	"context"
	"testing"

	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/httpapi"
)

func TestRunOutcomePreservesWaitingAndRunning(t *testing.T) {
	for state, want := range map[string]string{"running": "working", "waiting_user": "waiting_user", "waiting_external": "waiting_external", "done": "done"} {
		t.Run(state, func(t *testing.T) {
			if got := uiStatusForDaemonOutcome(state); got != want {
				t.Fatalf("Run %s displayed as %s, want %s", state, got, want)
			}
		})
	}
}

func TestWatchedOutcomeIsNotAlwaysCompletion(t *testing.T) {
	for _, state := range []string{"waiting_user", "waiting_external", "interrupted", ""} {
		m := NewController("", "", nil, "").model
		m.watchingRun, m.watchedRunID = true, "watched"
		m.updateInner(MsgAttachedRunDone{RunID: "watched", Status: state, Summary: "recorded state"})
		if m.runStatus == "done" || m.watchingRun {
			t.Fatalf("%q: observation became completion", state)
		}
	}
}

func TestLateParentReplyPreservesStoppedChildOutcome(t *testing.T) {
	m := NewController("", "", nil, "").model
	m.updateInner(MsgDaemonRunStarted{RunID: "child", Presentation: "foreground"})
	m.updateInner(MsgDaemonRunFinished{RunID: "child", Status: "waiting_user", Summary: "Waiting for confirmation"})
	m.updateInner(MsgAgentDone{Response: "Old parent result", Turn: &api.TurnStatus{RunID: "parent", Status: "done", TaskStatus: "done"}})
	if m.runStatus != "waiting_user" || m.daemonRunActive {
		t.Fatalf("late reply overwrote waiting child: %s", m.runStatus)
	}
}

func TestExplicitWatchFollowsOnlyExactContinuation(t *testing.T) {
	for _, source := range []string{"watched", "unrelated"} {
		m := NewController("", "", nil, "").model
		m.channel = "this-session"
		m.watchingRun, m.watchedRunID = true, "watched"
		m.runWatcher = func(context.Context, string, httpapi.StreamObserver) (api.RunObservation, error) {
			return api.RunObservation{}, nil
		}
		m.updateInner(MsgDaemonRunStarted{RunID: "child", ResumesRunID: source, Presentation: "foreground", Event: uiEventRef{Channel: "other-session"}})
		if source == "watched" {
			if m.watchedRunID != "child" {
				t.Fatal("exact continuation was not observed")
			}
			m.updateInner(MsgAttachedRunDone{RunID: "watched", Status: "waiting_user"})
			if m.watchedRunID != "child" || m.runStatus != "working" {
				t.Fatal("late observer changed child")
			}
		} else if m.watchedRunID != "watched" {
			t.Fatal("unrelated Run took observation")
		}
	}
}

func TestOldWatchCannotCompleteLiveContinuation(t *testing.T) {
	m := NewController("", "", nil, "").model
	m.watchingRun, m.watchedRunID = true, "parent"
	m.updateInner(MsgDaemonRunStarted{RunID: "child", Presentation: "foreground"})
	m.activePlanJSON = sessionTestPlan
	m.updateInner(MsgAttachedRunDone{RunID: "parent", Summary: "Waiting for approval"})
	if !m.daemonRunActive || m.daemonRunID != "child" || m.runStatus != "working" || m.activePlanJSON != sessionTestPlan {
		t.Fatalf("late parent observation overwrote child: active=%v run=%s UI=%s plan=%q", m.daemonRunActive, m.daemonRunID, m.runStatus, m.activePlanJSON)
	}
}

func TestControlReplyPreservesActiveRunAndProgress(t *testing.T) {
	for _, turn := range []*api.TurnStatus{nil, {Status: "completed"}} {
		m := NewController("", "", nil, "").model
		m.updateInner(MsgDaemonRunStarted{RunID: "live", Presentation: "foreground"})
		m.activePlanJSON = sessionTestPlan
		m.updateInner(MsgAgentDone{Response: "Approval recorded.", Input: "/approve", Turn: turn})
		m.updateInner(MsgToolStart{ToolName: "read_file", ToolCallID: "actual-call", Args: "{}", Event: uiEventRef{Source: eventSourceDaemon, RunID: "live"}})
		if !m.daemonRunActive || m.runStatus != "working" || m.toolExecuting != "read_file" || m.activePlanJSON != sessionTestPlan {
			t.Fatalf("control reply hid live work: active=%v UI=%s tool=%q plan=%q", m.daemonRunActive, m.runStatus, m.toolExecuting, m.activePlanJSON)
		}
	}
}

func TestWaitingOutcomeSurvivesHTTPAndEventOrder(t *testing.T) {
	for _, eventFirst := range []bool{false, true} {
		m := NewController("", "", nil, "").model
		m.localRequestActive, m.localRequestInput = true, "inspect the target"
		m.updateInner(MsgDaemonRunStarted{RunID: "current", Input: "inspect the target", Presentation: "foreground"})
		reply := MsgAgentDone{Response: "Inspection passed. Waiting for confirmation.", Input: "inspect the target", Turn: &api.TurnStatus{Status: "waiting_user", TaskStatus: "waiting_user", RunID: "current"}}
		end := MsgDaemonRunFinished{RunID: "current", Status: "waiting_user", Summary: "Waiting for confirmation."}
		if eventFirst {
			m.updateInner(end)
			m.updateInner(reply)
		} else {
			m.updateInner(reply)
			m.updateInner(end)
		}
		if m.runStatus != "waiting_user" {
			t.Fatalf("eventFirst=%v: waiting Run displayed as %s", eventFirst, m.runStatus)
		}
	}
}

func TestOldWatchCannotOverwriteAlreadyParkedChild(t *testing.T) {
	m := NewController("", "", nil, "").model
	m.watchingRun, m.watchedRunID = true, "parent"
	m.updateInner(MsgDaemonRunStarted{RunID: "child", Presentation: "foreground"})
	m.updateInner(MsgDaemonRunFinished{RunID: "child", Status: "waiting_user", Summary: "Please confirm"})
	m.updateInner(MsgAttachedRunDone{RunID: "parent", Status: "done", Summary: "old result"})
	if m.runStatus != "waiting_user" || m.watchingRun {
		t.Fatalf("old observer overwrote parked child: %s", m.runStatus)
	}
}

func TestNewHTTPOutcomeAcceptedWhenRunStartedEventWasMissed(t *testing.T) {
	m := NewController("", "", nil, "").model
	m.daemonRunID, m.runStatus = "previous", "waiting_user"
	m.messageProcessor = func(context.Context, api.MessageRequest) (api.MessageResponse, int) {
		return api.MessageResponse{Content: "New work complete", Turn: &api.TurnStatus{RunID: "new-run", Status: "done", TaskStatus: "done"}}, 200
	}
	cmd := m.runAgent(context.Background(), "new work")
	m.localRequestActive, m.localRequestInput = true, "new work"
	m.updateInner(cmd())
	if m.runStatus != "done" || m.daemonRunID != "new-run" {
		t.Fatalf("new recorded outcome rejected: %s %s", m.runStatus, m.daemonRunID)
	}
	m.updateInner(MsgAgentDone{PreviousRunID: "", Response: "old outcome", Turn: &api.TurnStatus{RunID: "previous", Status: "waiting_user", TaskStatus: "waiting_user"}})
	if m.runStatus != "done" || m.daemonRunID != "new-run" {
		t.Fatal("late old response replaced new outcome")
	}
}
