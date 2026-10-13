package httpapi

import (
	"fmt"
	"strings"
	"testing"

	"selfmind/internal/control"
)

// Events here have the shape the daemon stores: provider.call.usage opens a
// model call, and the tool.completed events after it are that call's step.
func failureUsage(run string, ms int, extra string) control.Event {
	return control.Event{RunID: run, Type: "provider.call.usage",
		Payload: []byte(fmt.Sprintf(`{"role":"coding_agent","input_tokens":10,"duration_ms":%d%s}`, ms, extra))}
}

func failureDone(run, tool, extra string) control.Event {
	return control.Event{RunID: run, Type: "tool.completed", Payload: []byte(fmt.Sprintf(`{"tool":%q,"invoked":true%s}`, tool, extra))}
}

func failureOf(run, tool, category string) control.Event {
	return failureDone(run, tool, fmt.Sprintf(`,"error":"boom","error_category":%q`, category))
}

func TestFailureClassNamesWhatTheCategorySays(t *testing.T) {
	for category, want := range map[string]string{
		"invalid_input": dailyFailureContract, "invalid_request": dailyFailureContract, "stale_precondition": dailyFailureContract,
		"policy": dailyFailureContract, "protocol": dailyFailureContract, "blocked_model_protocol": dailyFailureContract,
		"tool_schema": dailyFailureContract, "workspace_scope": dailyFailureContract,
		"command_failed": dailyFailureCommand, "timeout": dailyFailureCommand, "not_found": dailyFailureCommand,
		"exec_not_found": dailyFailureCommand, "syntax": dailyFailureCommand, "sandbox_no_network": dailyFailureCommand,
		"auth": dailyFailureCommand,
		// Not guessed: no declared category, a user decision, a store fault, and one nobody has named yet.
		"unknown": dailyFailureOther, "rejected": dailyFailureOther, "data_store": dailyFailureOther, "invented_later": dailyFailureOther,
	} {
		if got := dailyFailureClass(category); got != want {
			t.Errorf("%s classed %q, want %q", category, got, want)
		}
	}
}

// What is not the model's failure is not counted as one: a person's decision
// to wait, a guardrail redirect (the report counts those apart) and a call the
// Plan boundary held (counted as held), and a success that carries no error.
func TestOnlyFailuresOfTheModelsWorkAreCounted(t *testing.T) {
	for name, tc := range map[string]struct {
		message, category, code string
		want                    bool
	}{
		"command failure":        {"exit status 1", "command_failed", "", true},
		"no category recorded":   {"something broke", "", "", true},
		"human wait":             {"waiting for approval", "human_wait", "", false},
		"policy redirect":        {"use the daemon path", "policy_redirect", "", false},
		"held by plan boundary":  {"held", "policy", "plan_boundary_held", false},
		"success carries no err": {"", "", "", false},
		"blank error text":       {"  ", "command_failed", "", false},
	} {
		if _, got := dailyCallFailure("terminal", tc.message, tc.category, tc.code); got != tc.want {
			t.Errorf("%s: counted=%v, want %v", name, got, tc.want)
		}
	}
	if failure, _ := dailyCallFailure("terminal", "boom", "", ""); failure.class != dailyFailureOther {
		t.Errorf("an error with no category is %q, not a class to guess", failure.class)
	}
}

// Ten model calls of 6 s each (0.1 min), exercising every way a step is or is
// not a failure, a retry or neither.
func failureScript() []control.Event {
	const ms = 6000
	events := []control.Event{
		failureUsage("a", 1*ms, ""), failureOf("a", "update_plan", "invalid_input"), // 1 contract failure
		failureUsage("a", 2*ms, ""), failureDone("a", "update_plan", ""), // 2 same tool again at once: retry
		failureUsage("a", 3*ms, ""), failureOf("a", "terminal", "command_failed"), // 3 command failure
		failureUsage("a", 4*ms, ""), failureDone("a", "read_file", ""), // 4 another tool: not a retry
		failureUsage("a", 5*ms, ""), failureOf("a", "terminal", "timeout"), failureOf("a", "read_file", "unknown"), // 5 mixed: other
		failureUsage("a", 6*ms, ""), failureDone("a", "terminal", ""), // 6 retry of 5
		failureUsage("a", 7*ms, ""),                                                   // 7 no tools
		failureUsage("a", 8*ms, ""), failureOf("a", "terminal", "stale_precondition"), // 8 contract failure after a quiet step
		failureUsage("a", 9*ms, ""),                                    // 9 no tools: separates 8 from 10
		failureUsage("a", 10*ms, ""), failureDone("a", "terminal", ""), // 10 not immediately after 8: not a retry
	}
	return events
}

func TestFailedStepsCostTheModelCallThatRequestedThem(t *testing.T) {
	got := collectDailyPerformance(failureScript()).StepFailures
	const tenth = int64(6000) // 0.1 min
	if got.Steps != 4 || got.MS != (1+3+5+8)*tenth {
		t.Fatalf("failed steps=%d ms=%d", got.Steps, got.MS)
	}
	if got.ClassSteps[dailyFailureContract] != 2 || got.ClassMS[dailyFailureContract] != (1+8)*tenth ||
		got.ClassSteps[dailyFailureCommand] != 1 || got.ClassMS[dailyFailureCommand] != 3*tenth ||
		got.ClassSteps[dailyFailureOther] != 1 || got.ClassMS[dailyFailureOther] != 5*tenth {
		t.Fatalf("classes: steps=%v ms=%v", got.ClassSteps, got.ClassMS)
	}
	if got.RetrySteps != 2 || got.RetryMS != (2+6)*tenth {
		t.Fatalf("immediate same-tool retries: %d steps %d ms", got.RetrySteps, got.RetryMS)
	}
	plan, terminal := got.ByTool["update_plan"], got.ByTool["terminal"]
	if plan == nil || terminal == nil || len(got.ByTool) != 2 {
		t.Fatalf("by tool: %+v", got.ByTool)
	}
	if plan.Steps != 1 || plan.RetrySteps != 1 || plan.MS != 1*tenth || plan.Classes[dailyFailureContract] != 1 {
		t.Fatalf("update_plan: %+v", plan)
	}
	if terminal.Steps != 3 || terminal.RetrySteps != 1 || terminal.MS != (3+5+8)*tenth ||
		terminal.Classes[dailyFailureCommand] != 1 || terminal.Classes[dailyFailureOther] != 1 || terminal.Classes[dailyFailureContract] != 1 {
		t.Fatalf("terminal: %+v", terminal)
	}
	if all := collectDailyPerformance(failureScript()); all.ModelMS != 55*tenth {
		t.Fatalf("the Run's own model time is the denominator: %d", all.ModelMS)
	}
}

func TestFailedStepReportNamesCostShareAndTools(t *testing.T) {
	rendered := collectDailyPerformance(failureScript()).render()
	for _, want := range []string{
		"Failed model steps: 4 (1.7 min, 30% of 5.5 min model time): contract 2 (0.9 min), command 1 (0.3 min), other 1 (0.5 min); retried with the same tool at once: 2 more steps (0.8 min)\n",
		"Failed steps by tool: terminal 3 (command 1, contract 1, other 1; retried 1; 1.6 min), update_plan 1 (contract 1; retried 1; 0.1 min)\n",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("report lacks %q in:\n%s", want, rendered)
		}
	}
	if none := collectDailyPerformance(nil).render(); !strings.Contains(none, "Failed model steps: none\n") {
		t.Errorf("a window without failures says so: %s", none)
	}
}

// The window can open mid-Run: a failure whose model call came before it has
// no cost to attribute, so it is left out rather than counted as free.
func TestFailureWhoseModelCallPrecedesTheWindowIsNotCounted(t *testing.T) {
	events := []control.Event{
		failureOf("a", "terminal", "command_failed"), // requested by a call before the window
		failureUsage("a", 6000, ""), failureOf("a", "terminal", "command_failed"),
		failureUsage("a", 12000, ""), failureDone("a", "terminal", ""),
	}
	f := collectDailyPerformance(events).StepFailures
	if f.Steps != 1 || f.MS != 6000 || f.RetrySteps != 1 || f.RetryMS != 12000 {
		t.Fatalf("only the step with an in-window model call counts: %+v", f)
	}
}

// A step that fails right after a failed step of the same tool is one more
// failure, not also a retry: the correction attempt is the step that stopped failing.
func TestAFailedStepAfterAFailedStepIsNotAlsoARetry(t *testing.T) {
	events := []control.Event{
		failureUsage("a", 6000, ""), failureOf("a", "update_plan", "invalid_input"),
		failureUsage("a", 12000, ""), failureOf("a", "update_plan", "stale_precondition"),
		failureUsage("a", 18000, ""), failureDone("a", "update_plan", ""),
	}
	f := collectDailyPerformance(events).StepFailures
	if f.Steps != 2 || f.MS != 18000 || f.RetrySteps != 1 || f.RetryMS != 18000 {
		t.Fatalf("two failures and the one step that corrected them: %+v", f)
	}
}

// Only the five tools whose failed steps cost most are named; the rest are in
// the totals, so the line stays short however many tools fail.
func TestFailedStepsByToolNamesTheCostliestFive(t *testing.T) {
	var events []control.Event
	for i, tool := range []string{"t1", "t2", "t3", "t4", "t5", "t6", "t7"} {
		events = append(events, failureUsage("a", (i+1)*6000, ""), failureOf("a", tool, "command_failed"))
	}
	rendered := collectDailyPerformance(events).render()
	line := "Failed steps by tool: t7 1 (command 1; 0.7 min), t6 1 (command 1; 0.6 min), t5 1 (command 1; 0.5 min), t4 1 (command 1; 0.4 min), t3 1 (command 1; 0.3 min)\n"
	if !strings.Contains(rendered, line) {
		t.Fatalf("by-tool line:\n%s\nwant %q", rendered, line)
	}
	if !strings.Contains(rendered, "Failed model steps: 7 (") {
		t.Fatalf("the totals still count every tool:\n%s", rendered)
	}
}

func TestStepFailureCostsStayWithTheRunsOwnSteps(t *testing.T) {
	events := []control.Event{
		failureUsage("a", 6000, ""),
		// Another Run's steps interleave; they neither join nor break this Run's retry.
		failureUsage("b", 12000, ""), failureOf("b", "terminal", "command_failed"),
		failureOf("a", "update_plan", "invalid_input"),
		// A sub-agent's call and failures, and a batch child's, are not this Run's steps.
		failureUsage("a", 99000, `,"delegated":true`),
		failureDone("a", "terminal", `,"delegated":true,"error":"x","error_category":"command_failed"`),
		failureDone("a", "read_file", `,"nested":true,"error":"x","error_category":"command_failed"`),
		// A call that was deferred locally made no provider request and is no model call.
		failureUsage("a", 7000, `,"status":"deferred","provider_dispatched":false`),
		failureUsage("a", 18000, ""), failureDone("a", "update_plan", ""),
		// An event with no Run is no Run's step.
		failureDone("", "terminal", `,"error":"x","error_category":"command_failed"`),
		// Neither a human wait nor a held call is a failure.
		failureUsage("a", 24000, ""), failureDone("a", "terminal", `,"error":"wait","error_category":"human_wait"`),
		failureDone("a", "read_file", `,"invoked":false,"error":"held","error_category":"policy","error_code":"plan_boundary_held"`),
	}
	got := collectDailyPerformance(events)
	f := got.StepFailures
	if f.Steps != 2 || f.MS != 12000+6000 {
		t.Fatalf("failed steps=%d ms=%d (Run b's terminal failure and Run a's own update_plan failure)", f.Steps, f.MS)
	}
	// The deferral between the failure and its retry does not make the retry non-adjacent.
	if f.RetrySteps != 1 || f.RetryMS != 18000 {
		t.Fatalf("retry steps=%d ms=%d", f.RetrySteps, f.RetryMS)
	}
	if want := int64(6000 + 12000 + 18000 + 24000); got.ModelMS != want {
		t.Fatalf("model ms=%d, want %d: deferred, delegated and Run-less calls are excluded", got.ModelMS, want)
	}
}
