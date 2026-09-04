package router

import (
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
)

// TestRecoveredToolFailuresAreNotBlamed replays a real run: the model
// prematurely claimed all steps done, finish_run was REFUSED by the
// verification guard, the model corrected course, fixed a wrong assertion, and
// its final verification passed — then it stopped without a final answer.
//
// Any failure anywhere in the turn used to trigger "SelfMind encountered a tool
// error before producing a final response. Review the tool events above",
// pointing the person at failures the model had already handled and implying
// something was broken. Only an UNRECOVERED failure earns that message.
func TestRecoveredToolFailuresAreNotBlamed(t *testing.T) {
	var s EventSummary
	s.Observe(llm.StreamEvent{EventType: "tool.started", ToolName: "finish_run"})
	s.Observe(llm.StreamEvent{EventType: "tool.completed", ToolName: "finish_run", Err: errString("unresolved plan steps")})
	s.Observe(llm.StreamEvent{EventType: "tool.started", ToolName: "verify"})
	s.Observe(llm.StreamEvent{EventType: "tool.completed", ToolName: "verify", Err: errString("exit status 1")})
	// The corrected check passes: the last tool to finish SUCCEEDED.
	s.Observe(llm.StreamEvent{EventType: "tool.started", ToolName: "verify"})
	s.Observe(llm.StreamEvent{EventType: "tool.completed", ToolName: "verify"})
	s.Observe(llm.StreamEvent{EventType: "turn.completed", Payload: map[string]interface{}{
		"status": "completed", "completion_reason": "completed", "resumable": false,
	}})

	got := s.WithContent("")
	if strings.Contains(got, "encountered a tool error") {
		t.Fatalf("recovered tool failures were blamed for the missing answer: %q", got)
	}
	if !strings.Contains(got, "without producing a final response") {
		t.Fatalf("the neutral notice should stand in: %q", got)
	}
}

// An unrecovered failure — the last tool errored and nothing followed — still
// reports the tool error, which is the one case that message is true.
func TestUnrecoveredToolFailureStillReportsIt(t *testing.T) {
	var s EventSummary
	s.Observe(llm.StreamEvent{EventType: "tool.started", ToolName: "terminal"})
	s.Observe(llm.StreamEvent{EventType: "tool.completed", ToolName: "terminal", Err: errString("boom")})

	got := s.WithContent("")
	if !strings.Contains(got, "encountered a tool error") {
		t.Fatalf("an unrecovered failure should still be reported: %q", got)
	}
}

// A resumable status the turn reports itself earns the actionable notice. Only
// "done"/"completed" are successes; the check used to be status == "incomplete",
// so verification_partial, waiting_user, interrupted and blocked all silently
// dropped their resume hint.
func TestResumableTurnStatusEarnsTheNotice(t *testing.T) {
	for _, status := range []string{"verification_partial", "waiting_user", "interrupted", "blocked"} {
		var s EventSummary
		s.Observe(llm.StreamEvent{EventType: "turn.completed", Payload: map[string]interface{}{
			"status": status, "completion_reason": "missing_final_response", "resumable": true,
		}})
		got := s.WithContent("the change is in place")
		if !strings.Contains(got, "stopped before full completion") {
			t.Fatalf("%s: notice missing: %q", status, got)
		}
		if !strings.Contains(got, `reply "continue" to resume`) {
			t.Fatalf("%s: resume path missing: %q", status, got)
		}
		if !strings.Contains(got, "the change is in place") {
			t.Fatalf("%s: the answer was dropped: %q", status, got)
		}
	}
}

// A clean run with a final answer keeps the answer untouched.
func TestSuccessfulOutcomeKeepsTheAnswer(t *testing.T) {
	var s EventSummary
	s.Observe(llm.StreamEvent{EventType: "turn.completed", Payload: map[string]interface{}{
		"status": "completed", "completion_reason": "completed",
	}})
	if got := s.WithContent("all three records backfilled"); got != "all three records backfilled" {
		t.Fatalf("a successful outcome must not decorate the answer: %q", got)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
