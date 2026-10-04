package cli

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/httpapi"
)

const sessionTestPlan = `{"explanation":"release","plan":[{"step":"publish the release","status":"completed"},{"step":"confirm with the owner","status":"in_progress"}]}`

// sessionRef is a daemon event as the stream delivers it for one session.
func sessionRef(runID, eventID, channel string, detail bool) uiEventRef {
	return uiEventRef{Source: eventSourceDaemon, RunID: runID, EventID: eventID, Channel: channel, Detail: detail}
}

// Window A published a task and waits for confirmation; window B then analyses
// code. A must keep its plan and state while B's run streams, and report B's
// work in one status line only. Every A window used to take B's run as its
// own: it cleared its plan and rendered B's progress.
func TestAnotherSessionsRunLeavesThisTerminalAlone(t *testing.T) {
	m, _ := newApprovalTestModel()
	m.channel = "session-a"
	m.applyPlanSnapshot(sessionTestPlan, uiEventRef{RunID: "run_a"})
	messages := len(m.messages)

	m.Update(MsgDaemonRunStarted{RunID: "run_b", Input: "analyse the parser", Event: sessionRef("run_b", "ev1", "session-b", false)})
	m.Update(MsgPlanUpdated{Content: `{"plan":[{"step":"read parser.go","status":"in_progress"}]}`, Event: sessionRef("run_b", "ev2", "session-b", true)})
	m.Update(MsgToolStart{ToolName: "read_file", ToolCallID: "call_1", Event: sessionRef("run_b", "ev3", "session-b", true)})
	m.Update(MsgStream{Content: "The parser", Event: sessionRef("run_b", "", "session-b", true)})

	if m.daemonRunActive || m.daemonRunID != "" || !strings.Contains(m.activePlanJSON, "confirm with the owner") {
		t.Fatalf("another session's run took this terminal: active=%v run=%q plan=%q", m.daemonRunActive, m.daemonRunID, m.activePlanJSON)
	}
	if len(m.messages) != messages {
		t.Fatalf("another session's progress entered this transcript: %+v", m.messages[messages:])
	}
	if live := stripANSI(m.viewActiveRegion()); strings.Contains(live, "The parser") || strings.Contains(live, "read_file") {
		t.Fatalf("another session's progress is live in this terminal:\n%s", live)
	}
	if !strings.Contains(m.statusMsg, "Other session: working on “analyse the parser”") {
		t.Fatalf("status = %q, want one line about the other session's work", m.statusMsg)
	}
	if got := m.foreignDetailEvents.Load(); got != 3 {
		t.Fatalf("dropped other-session detail events = %d, want 3", got)
	}

	// Asking to watch it is the only way its progress appears here.
	m.clientMode = true
	m.runWatcher = func(context.Context, string, httpapi.StreamObserver) string { return "" }
	m.handleAttach(nil)
	if !m.watchingRun || m.watchedRunID != "run_b" {
		t.Fatalf("/attach did not watch the other session's run: watching=%v run=%q", m.watchingRun, m.watchedRunID)
	}
	watched := uiEventRef{Source: eventSourceWatch, RunID: "run_b", EventID: "ev4", Channel: "session-b", Detail: true}
	if !m.acceptEvent(watched) {
		t.Fatal("an attached run's progress was dropped")
	}

	m.Update(MsgDaemonRunFinished{RunID: "run_b", Status: "done", Event: sessionRef("run_b", "ev5", "session-b", false)})
	m.Update(MsgAttachedRunDone{RunID: "run_b"})
	if m.otherRunID != "" {
		t.Fatalf("the other session's finished run is still reported: %q", m.otherRunID)
	}

	// A run the daemon started for that session is named by its origin, not
	// by the daemon's own instruction.
	m.Update(MsgDaemonRunStarted{RunID: "run_c", Origin: "watch", Input: "Resume the original task after a durable external watch", Event: sessionRef("run_c", "ev6", "session-b", false)})
	if !strings.Contains(m.statusMsg, "“background run (watch)”") || strings.Contains(m.statusMsg, "Resume the original") {
		t.Fatalf("status = %q, want the background run named by its origin", m.statusMsg)
	}
}

// The same events for this terminal's own run drive it as before.
func TestThisSessionsRunStillDrivesThisTerminal(t *testing.T) {
	m, _ := newApprovalTestModel()
	m.channel = "session-a"
	m.Update(MsgDaemonRunStarted{RunID: "run_a", Input: "publish", Event: sessionRef("run_a", "ev1", "session-a", false)})
	m.Update(MsgPlanUpdated{Content: sessionTestPlan, Event: sessionRef("run_a", "ev2", "session-a", true)})
	if !m.daemonRunActive || m.daemonRunID != "run_a" || !strings.Contains(m.activePlanJSON, "confirm with the owner") {
		t.Fatalf("own run: active=%v run=%q plan=%q", m.daemonRunActive, m.daemonRunID, m.activePlanJSON)
	}
	if m.foreignDetailEvents.Load() != 0 || m.otherRunID != "" {
		t.Fatal("this terminal's own run was treated as another session's")
	}
}

// Only the session whose run asks arms the approval panel or the question
// prompt; another terminal says what waits and how to answer it from there,
// and withdraws that line once the approval is answered anywhere.
func TestAnotherSessionsHumanWaitsAreReportedNotArmed(t *testing.T) {
	m, _ := newApprovalTestModel()
	m.channel = "session-b"
	m.Update(MsgApprovalRequest{ID: "apr_a", Tool: "terminal", Target: "git push", Channel: "session-a"})
	if m.approvalPrompt != nil || m.approvalFlowActive() {
		t.Fatal("another session's approval armed a panel here")
	}
	if !strings.Contains(m.statusMsg, "approval waiting for terminal → git push") || !strings.Contains(m.statusMsg, "/approve") {
		t.Fatalf("status = %q, want the waiting approval and how to answer it", m.statusMsg)
	}
	m.Update(MsgApprovalResolved{ID: "apr_a", Status: "approved", Event: uiEventRef{EventID: "ev_resolved"}})
	if strings.Contains(m.statusMsg, "approval waiting") {
		t.Fatalf("an answered approval is still reported: %q", m.statusMsg)
	}

	m.Update(MsgClarifyRequest{ID: "clr_a", Question: "Which bucket?", Channel: "session-a"})
	if m.clarifyMode {
		t.Fatal("another session's question captured this terminal's input")
	}
	if !strings.Contains(m.statusMsg, "Which bucket?") || !strings.Contains(m.statusMsg, "Answer it in that session") {
		t.Fatalf("status = %q, want the waiting question and where to answer it", m.statusMsg)
	}

	m.Update(MsgApprovalRequest{ID: "apr_b", Tool: "terminal", Target: "make test", Channel: "session-b"})
	if m.approvalPrompt == nil {
		t.Fatal("this session's own approval did not arm its panel")
	}
}

// An exact reply edge may send another session's message to that Run; the
// terminal says where it went instead of flashing a generic notice.
func TestMessageSentToAnotherSessionsRunGetsAReceipt(t *testing.T) {
	m, _ := newApprovalTestModel()
	m.channel = "session-b"
	m.Update(MsgDaemonRunStarted{RunID: "run_a", Input: "publish the release", Event: sessionRef("run_a", "ev1", "session-a", false)})
	m.localRequestActive = true
	m.localRequestInput = "also update the changelog"
	m.Update(MsgAgentDone{Input: "also update the changelog", Response: "Sent.", Turn: &api.TurnStatus{Status: "accepted", RunID: "run_a", Message: "publish the release"}})
	last := m.messages[len(m.messages)-1]
	if last.Role != "notice" || !strings.Contains(last.Content, "“publish the release”, which is running in another session") {
		t.Fatalf("last message = %+v, want a receipt naming the other session's task", last)
	}
}

// A terminal opened while another session works reports that work and its
// waiting approval; it restores neither that run's plan nor its panel.
func TestStartupDigestReportsAnotherSessionsWork(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.width, model.height = 100, 30
	model.clientMode = true
	model.channel = "session-b"
	model.runWatcher = func(context.Context, string, httpapi.StreamObserver) string { return "" }
	model.startupDigest = &api.DigestResponse{
		ActiveRun:        &api.DigestActiveRun{RunID: "run_a", Channel: "session-a", Title: "gcp release", PlanJSON: sessionTestPlan},
		PendingApprovals: []api.DigestApproval{{ID: "apr_a", Channel: "session-a", Tool: "terminal", Target: "git push", Line: "[terminal] git push"}},
	}

	model.maybeShowStartupDigest(model.width)

	if model.watchingRun || strings.TrimSpace(model.activePlanJSON) != "" || model.approvalPrompt != nil {
		t.Fatalf("attached to another session's work: watching=%v plan=%q panel=%v", model.watchingRun, model.activePlanJSON, model.approvalPrompt != nil)
	}
	digest := model.messages[len(model.messages)-1].Content
	for _, want := range []string{"Another session is running: gcp release", "1 approval waiting in another session"} {
		if !strings.Contains(digest, want) {
			t.Fatalf("digest missing %q:\n%s", want, digest)
		}
	}
	if model.otherRunID != "run_a" || !strings.Contains(model.statusMsg, "approval waiting") {
		t.Fatalf("other session not reported: run=%q status=%q", model.otherRunID, model.statusMsg)
	}
}

// A sub-agent's tool calls show in the parent run's transcript marked as the
// sub-agent's, so the person can tell which agent read or ran what.
func TestSubAgentToolCallsAreMarked(t *testing.T) {
	m, _ := newApprovalTestModel()
	m.Update(MsgDaemonRunStarted{RunID: "run_a", Input: "delegate", Event: sessionRef("run_a", "ev0", "", false)})
	m.Update(MsgToolStart{ToolName: "read_file", ToolCallID: "sub_1", Args: `{"path":"notes.txt"}`, Delegated: true, Event: sessionRef("run_a", "ev1", "", true)})
	m.Update(MsgToolStart{ToolName: "read_file", ToolCallID: "own_1", Args: `{"path":"plan.md"}`, Event: sessionRef("run_a", "ev2", "", true)})
	live := stripANSI(m.viewActiveRegion())
	marked, unmarked := false, true
	for _, line := range strings.Split(live, "\n") {
		if strings.Contains(line, "notes.txt") && strings.Contains(line, "sub-agent") {
			marked = true
		}
		if strings.Contains(line, "plan.md") && strings.Contains(line, "sub-agent") {
			unmarked = false
		}
	}
	if !marked || !unmarked {
		t.Fatalf("sub-agent marking wrong (marked=%v, own call unmarked=%v):\n%s", marked, unmarked, live)
	}
}
