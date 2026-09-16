package cli

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
)

// A background run still exposes approvals, but answering one must not create
// foreground activity that its deliberately quiet completion path cannot end.
func TestBackgroundApprovalDoesNotLeaveWorkingAfterCompletion(t *testing.T) {
	for _, origin := range []string{"watch", "cron"} {
		for _, decision := range []string{"none", "approve", "deny", "remote_approve", "remote_deny"} {
			t.Run(origin+"/"+decision, func(t *testing.T) {
				m, calls := newApprovalTestModel()
				start := MsgDaemonRunStarted{RunID: "run_background", Origin: origin}
				if origin == "watch" {
					start.WatchID = "watch_build"
				}
				m.Update(start)
				if decision != "none" {
					m.Update(MsgApprovalRequest{ID: "apr_background", Tool: "patch"})
					if m.approvalPrompt == nil {
						t.Fatal("background approval must remain answerable")
					}
					switch decision {
					case "approve":
						m.Update(keyRunes("y"))
					case "deny":
						m.Update(keyRunes("n"))
					case "remote_approve":
						m.Update(MsgApprovalResolved{ID: "apr_background", Status: "approved"})
					case "remote_deny":
						m.Update(MsgApprovalResolved{ID: "apr_background", Status: "rejected"})
					}
					if m.approvalPrompt != nil {
						t.Fatal("resolved approval left its panel open")
					}
					if (decision == "approve" || decision == "deny") && len(*calls) != 1 {
						t.Fatalf("decision was not sent exactly once: %+v", *calls)
					}
				}
				if row := m.activityRow(100); row != "" {
					t.Errorf("background approval started foreground activity: %q", stripANSI(row))
				}
				m.Update(MsgDaemonRunFinished{RunID: start.RunID, Status: "done", Summary: "Record verified."})
				if m.runStatus != "done" || m.daemonRunActive {
					t.Fatalf("run did not finish: status=%s active=%v", m.runStatus, m.daemonRunActive)
				}
				if row := m.activityRow(100); row != "" {
					t.Errorf("completed run still renders activity: %q", stripANSI(row))
				}
				for _, tick := range []tea.Msg{MsgWorkingTick{}, spinner.TickMsg{}} {
					if _, cmd := m.Update(tick); cmd != nil {
						t.Errorf("completed run rearmed %T", tick)
					}
				}
				if last := m.messages[len(m.messages)-1]; !strings.Contains(last.Content, "Record verified.") {
					t.Fatalf("background result missing: %+v", last)
				}
			})
		}
	}
}

func TestBackgroundApprovalPreservesOutstandingLocalRequest(t *testing.T) {
	m, _ := newApprovalTestModel()
	m.Update(MsgDaemonRunStarted{RunID: "run_background", Origin: "watch", WatchID: "watch_build"})
	m.localRequestActive = true
	m.localRequestInput = "new request"
	m.startModelWait("Waiting for request acknowledgement")
	m.Update(MsgApprovalRequest{ID: "apr_background", Tool: "patch"})
	m.Update(keyRunes("y"))
	m.Update(MsgDaemonRunFinished{RunID: "run_background", Status: "done"})
	if !m.localRequestActive || m.activityRow(100) == "" {
		t.Fatal("background completion cleared an outstanding local request")
	}
	m.Update(MsgAgentDone{Response: "Request acknowledged."})
	if m.activityRow(100) != "" {
		t.Fatal("local request completion left activity behind")
	}
}

func TestLateBackgroundCompletionPreservesApprovedForegroundActivity(t *testing.T) {
	m, _ := newApprovalTestModel()
	m.Update(MsgDaemonRunStarted{RunID: "run_background", Origin: "watch", WatchID: "watch_build"})
	m.queuedRunIDs = []string{"queue_foreground"}
	m.queuedCount = 1
	m.Update(MsgDaemonRunStarted{RunID: "run_foreground", QueueID: "queue_foreground"})
	m.Update(MsgApprovalRequest{ID: "apr_foreground", Tool: "terminal"})
	m.Update(keyRunes("y"))
	if !m.thinking || m.activityRow(100) == "" {
		t.Fatal("foreground approval must resume its activity")
	}
	m.Update(MsgDaemonRunFinished{RunID: "run_background", Status: "done"})
	if m.daemonRunID != "run_foreground" || !m.thinking || m.activityRow(100) == "" {
		t.Fatal("late background completion stopped foreground activity")
	}
	m.Update(MsgDaemonRunFinished{RunID: "run_foreground", Status: "done"})
	if m.activityRow(100) != "" {
		t.Fatal("foreground completion left activity behind")
	}
}
