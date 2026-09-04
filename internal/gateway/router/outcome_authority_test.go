package router

import (
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
)

// TestRunOutcomeOutranksTheTurnsSelfReport replays a real run: the model
// prematurely claimed all steps done, finish_run was REFUSED by the
// verification guard, the model corrected course, fixed a wrong assertion, and
// its final verification passed — then it stopped without a final answer.
//
// The turn reported itself "completed" while the run resolved to
// verification_partial / missing_final_response. Reading the turn instead of
// the run told the person a tool had errored, pointed them at recovered
// failures, and dropped the "reply continue" the outcome had already computed.
func TestRunOutcomeOutranksTheTurnsSelfReport(t *testing.T) {
	var s EventSummary
	s.Observe(llm.StreamEvent{EventType: "tool.started", ToolName: "finish_run"})
	s.Observe(llm.StreamEvent{EventType: "tool.completed", ToolName: "finish_run", Err: errString("unresolved plan steps")})
	s.Observe(llm.StreamEvent{EventType: "tool.started", ToolName: "verify"})
	s.Observe(llm.StreamEvent{EventType: "tool.completed", ToolName: "verify", Err: errString("exit status 1")})
	// The corrected check passes: the last tool to finish SUCCEEDED.
	s.Observe(llm.StreamEvent{EventType: "tool.started", ToolName: "verify"})
	s.Observe(llm.StreamEvent{EventType: "tool.completed", ToolName: "verify"})
	s.Observe(llm.StreamEvent{EventType: "run.outcome", Payload: map[string]interface{}{
		"status":            "verification_partial",
		"completion_reason": "missing_final_response",
		"resumable":         true,
	}})
	s.Observe(llm.StreamEvent{EventType: "turn.completed", Payload: map[string]interface{}{
		"status":            "completed",
		"completion_reason": "completed",
		"resumable":         false,
	}})

	got := s.WithContent("")
	if strings.Contains(got, "encountered a tool error") {
		t.Fatalf("recovered tool failures were blamed for the missing answer: %q", got)
	}
	if !strings.Contains(got, "stopped before full completion") {
		t.Fatalf("the actionable notice is missing: %q", got)
	}
	if !strings.Contains(got, "missing final response") {
		t.Fatalf("notice should name the real reason: %q", got)
	}
	if !strings.Contains(got, `reply "continue" to resume`) {
		t.Fatalf("notice should carry the resume path the outcome computed: %q", got)
	}
}

// A genuinely unrecovered failure — the last tool errored and nothing followed —
// still reports the tool error, which is the one case that message is true.
func TestUnrecoveredToolFailureStillReportsIt(t *testing.T) {
	var s EventSummary
	s.Observe(llm.StreamEvent{EventType: "tool.started", ToolName: "terminal"})
	s.Observe(llm.StreamEvent{EventType: "tool.completed", ToolName: "terminal", Err: errString("boom")})

	got := s.WithContent("")
	if !strings.Contains(got, "encountered a tool error") {
		t.Fatalf("an unrecovered failure should still be reported: %q", got)
	}
}

// A clean run with a final answer keeps the answer untouched.
func TestSuccessfulOutcomeKeepsTheAnswer(t *testing.T) {
	var s EventSummary
	s.Observe(llm.StreamEvent{EventType: "run.outcome", Payload: map[string]interface{}{
		"status": "done", "completion_reason": "completed",
	}})
	if got := s.WithContent("all three records backfilled"); got != "all three records backfilled" {
		t.Fatalf("a successful outcome must not decorate the answer: %q", got)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
