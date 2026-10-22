package cli

import (
	"strings"
	"testing"
)

func TestForeignRunFinishPreservesOtherQuestion(t *testing.T) {
	for _, finish := range []string{"run_question", "run_independent"} {
		t.Run(finish, func(t *testing.T) {
			m, _ := newApprovalTestModel()
			m.channel = "review-local"
			m.Update(MsgDaemonRunStarted{RunID: "run_question", Input: "work needing a decision", Event: sessionRef("run_question", "review-1", "session-question", false)})
			if finish == "run_independent" {
				m.Update(MsgDaemonRunStarted{RunID: finish, Input: "independent work", Event: sessionRef(finish, "review-2", "session-independent", false)})
			}
			m.Update(MsgClarifyRequest{ID: "review-question", Question: "Which acceptance target?", Channel: "session-question", RunID: "run_question"})
			if m.otherClarifyID != "review-question" || !strings.Contains(m.statusMsg, "question waiting") {
				t.Fatalf("missing initial wait: id=%q notice=%q", m.otherClarifyID, m.statusMsg)
			}
			channel := "session-question"
			if finish == "run_independent" {
				channel = "session-independent"
			}
			m.Update(MsgDaemonRunFinished{RunID: finish, Status: "done", Event: sessionRef(finish, "review-4", channel, false)})
			if finish == "run_question" {
				if m.otherClarifyID != "" {
					t.Fatal("finished question run kept stale wait")
				}
			} else if m.otherClarifyID != "review-question" || !strings.Contains(m.statusMsg, "question waiting") {
				t.Fatalf("independent completion erased pending question: id=%q notice=%q", m.otherClarifyID, m.statusMsg)
			}
		})
	}
}

func TestForeignQuestionsResolveIndependently(t *testing.T) {
	m, _ := newApprovalTestModel()
	m.channel = "local"
	for _, id := range []string{"a", "b"} {
		m.Update(MsgClarifyRequest{ID: id, RunID: "run_" + id, Question: "Question " + id, Channel: "session_" + id})
	}
	m.Update(MsgDaemonRunFinished{RunID: "run_b", Status: "waiting_user", Event: sessionRef("run_b", "park_b", "session_b", false)})
	if len(m.otherClarifications) != 2 {
		t.Fatal("parking erased a pending human wait")
	}
	m.Update(MsgClarifyResolved{ID: "a"})
	if m.otherClarifyID != "b" || !strings.Contains(m.statusMsg, "Question b") {
		t.Fatal("resolving one question hid the other")
	}
	m.Update(MsgDaemonRunFinished{RunID: "run_b", Status: "done", Event: sessionRef("run_b", "done_b", "session_b", false)})
	if len(m.otherClarifications) != 0 || m.otherClarifyID != "" {
		t.Fatal("settled exact run kept its question")
	}
}
