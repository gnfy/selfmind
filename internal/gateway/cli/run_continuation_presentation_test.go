package cli

import (
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel/llm"
	"strings"
	"testing"
)

func TestProviderResumeKeepsOwnSessionForegroundAndCompleteAnswer(t *testing.T) {
	for _, replay := range []bool{false, true} {
		m, _ := newApprovalTestModel()
		m.channel = "session-a"
		m.Update(MsgDaemonRunStarted{RunID: "child", Origin: "provider_wait", Presentation: "foreground", Input: "internal resume", Event: sessionRef("child", "start", "session-a", false)})
		if !m.daemonRunOwned || m.backgroundRunEvent(sessionRef("child", "", "session-a", true)) {
			t.Fatal("provider continuation lost interactive ownership")
		}
		answer := "Conclusion\n" + strings.Repeat("Observed fact. ", 200) + "\nUnfinished: none."
		if !replay {
			m.Update(MsgStream{Content: "partial commentary", Phase: llm.AssistantPhaseCommentary, Event: sessionRef("child", "", "session-a", true)})
		}
		m.Update(MsgDaemonRunFinished{RunID: "child", Status: "done", Summary: "short summary", FinalAnswer: answer, Event: sessionRef("child", "finish", "session-a", false)})
		count := 0
		for _, msg := range m.messages {
			if msg.Role == "assistant" && msg.Content == answer {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("complete answer count=%d (replay=%v)", count, replay)
		}
		m.Update(MsgDaemonRunFinished{RunID: "child", Status: "done", FinalAnswer: answer, Event: sessionRef("child", "finish", "session-a", false)})
		count = 0
		for _, msg := range m.messages {
			if msg.Content == answer {
				count++
			}
		}
		if count != 1 {
			t.Fatal("terminal replay duplicated the answer")
		}
	}
}

func TestForeignProviderResumeCannotTakeForeground(t *testing.T) {
	m, _ := newApprovalTestModel()
	m.channel = "session-a"
	m.Update(MsgDaemonRunStarted{RunID: "foreign", Presentation: "foreground", Origin: "provider_wait", Event: sessionRef("foreign", "start", "session-b", false)})
	m.Update(MsgDaemonRunFinished{RunID: "foreign", FinalAnswer: "private conclusion", Status: "done", Event: sessionRef("foreign", "finish", "session-b", false)})
	if m.daemonRunOwned || m.daemonRunActive {
		t.Fatal("foreign continuation took the terminal")
	}
	for _, msg := range m.messages {
		if strings.Contains(msg.Content, "private conclusion") {
			t.Fatal("foreign answer leaked")
		}
	}
}

func TestLateParentReplyDoesNotFinalizeChildStream(t *testing.T) {
	m, _ := newApprovalTestModel()
	m.channel = "session-a"
	m.Update(MsgDaemonRunStarted{RunID: "child", Presentation: "foreground", Origin: "provider_wait", Event: sessionRef("child", "start", "session-a", false)})
	m.Update(MsgStream{Content: "child live output", Event: sessionRef("child", "", "session-a", true)})
	m.Update(MsgAgentDone{Response: "Waiting for provider", Turn: &api.TurnStatus{RunID: "parent"}})
	if m.daemonRunID != "child" || !m.daemonRunOwned || !strings.Contains(m.processState().previewContent(), "child live output") {
		t.Fatal("late parent reply erased child progress")
	}
}
