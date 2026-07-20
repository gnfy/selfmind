package eval

import "testing"

func TestEvaluateCaseDetectsMojibakeAndRawJSONLeak(t *testing.T) {
	c := &Case{
		ID:    "bad_output",
		Turns: []Turn{{Input: "hello"}},
		Checks: CheckSettings{
			NoMojibake:      true,
			NoRawJSONLeak:   true,
			NoToolXMLLeak:   true,
			NoEmptyResponse: true,
		},
	}
	checks := EvaluateCase(c, RunSnapshot{Output: `{"tool_calls":[{"name":"x"}]} ��� <tool>`})
	byName := map[string]CheckResult{}
	for _, check := range checks {
		byName[check.Name] = check
	}
	if byName["no_empty_response"].OK != true {
		t.Fatalf("non-empty output should pass no_empty_response: %+v", byName["no_empty_response"])
	}
	for _, name := range []string{"no_mojibake", "no_raw_json_leak", "no_tool_xml_leak"} {
		if byName[name].OK {
			t.Fatalf("%s should fail: %+v", name, byName[name])
		}
	}
}

func TestHasMojibakeDetectsCommonChineseEncodingArtifacts(t *testing.T) {
	if !hasMojibake("鍒嗘瀽涓€涓嬪綋鍓?selfmind 椤圭洰") {
		t.Fatalf("common UTF-8/GBK mojibake should be detected")
	}
	if hasMojibake("分析一下当前 selfmind 项目") {
		t.Fatalf("normal Chinese text should not be treated as mojibake")
	}
}

func TestEvaluateCaseRequiresToolEvents(t *testing.T) {
	c := &Case{
		ID:    "tool_case",
		Turns: []Turn{{Input: "inspect code"}},
		Expect: Expectations{
			RequireToolEvents: true,
			MinToolCalls:      2,
		},
		Checks: CheckSettings{NoEmptyResponse: true},
	}
	checks := EvaluateCase(c, RunSnapshot{Output: "done", ToolCalls: 1, ActionToolCalls: 1})
	byName := map[string]CheckResult{}
	for _, check := range checks {
		byName[check.Name] = check
	}
	if !byName["require_tool_events"].OK {
		t.Fatalf("one tool call should satisfy require_tool_events")
	}
	if byName["min_tool_calls"].OK {
		t.Fatalf("one tool call should not satisfy min_tool_calls=2")
	}
}

func TestEvaluateCaseMaxToolCallsUsesActionTools(t *testing.T) {
	max := 0
	c := &Case{
		ID:    "direct",
		Turns: []Turn{{Input: "who are you"}},
		Expect: Expectations{
			MaxToolCalls: &max,
		},
		Checks: CheckSettings{NoEmptyResponse: true},
	}
	checks := EvaluateCase(c, RunSnapshot{Output: "done", ToolCalls: 1, ActionToolCalls: 0})
	for _, check := range checks {
		if check.Name == "max_tool_calls" && !check.OK {
			t.Fatalf("lifecycle tools should not fail max_tool_calls: %+v", check)
		}
	}
	checks = EvaluateCase(c, RunSnapshot{Output: "done", ToolCalls: 2, ActionToolCalls: 1})
	for _, check := range checks {
		if check.Name == "max_tool_calls" && check.OK {
			t.Fatalf("action tool should fail max_tool_calls=0: %+v", check)
		}
	}
}

func TestContextOverflowIgnoresNormalArchitectureText(t *testing.T) {
	if hasContextOverflow("The context window is selected by the context engine.", nil) {
		t.Fatalf("normal architecture discussion should not count as context overflow")
	}
	if hasContextOverflow("Provider profiles include context length, max tokens, and fallback models.", nil) {
		t.Fatalf("normal provider capability discussion should not count as context overflow")
	}
	if hasContextOverflow("Provider docs can describe a model's maximum context length.", nil) {
		t.Fatalf("normal maximum context length discussion should not count as context overflow")
	}
}

func TestContextOverflowDetectsProviderError(t *testing.T) {
	if !hasContextOverflow("", []string{"provider failed: too many tokens for context length"}) {
		t.Fatalf("provider context error should be detected")
	}
}

func TestClassifyErrorDoesNotTreatDiagnosticContextAsOverflow(t *testing.T) {
	got := classifyError("SelfMind diagnostic instruction: inspect relevant context such as cwd and files; command failed: exit status 127")
	if got != "command_failed" {
		t.Fatalf("diagnostic context text should not be context_overflow, got %q", got)
	}
}

func TestCompletedStatusAcceptsSuccessfulTurnEvenWhenTaskStillRunning(t *testing.T) {
	c := &Case{
		ID:    "long_lived_task",
		Turns: []Turn{{Input: "inspect"}},
		Expect: Expectations{
			Status: "completed",
		},
	}
	checks := EvaluateCase(c, RunSnapshot{Output: "done", OutcomeStatus: "completed"})
	for _, check := range checks {
		if check.Name == "status:completed" && !check.OK {
			t.Fatalf("completed turn should satisfy completed status: %+v", check)
		}
	}
}

func TestEvaluateCaseChecksStructuredCompletionAndVerification(t *testing.T) {
	resumable := false
	c := &Case{
		ID:    "verified_completion",
		Turns: []Turn{{Input: "change and verify"}},
		Expect: Expectations{
			Status:            "completed",
			CompletionReason:  "completed",
			Resumable:         &resumable,
			VerificationState: "passed",
		},
	}
	checks := EvaluateCase(c, RunSnapshot{
		Output:            "done",
		OutcomeStatus:     "completed",
		CompletionReason:  "completed",
		Resumable:         false,
		VerificationState: "passed",
	})
	if !ChecksPassed(checks) {
		t.Fatalf("structured completion checks should pass: %+v", checks)
	}
}

func TestEvaluateCaseAcceptsGatewayRejectionBeforeTaskCreation(t *testing.T) {
	c := &Case{
		ID:    "gateway_rejection",
		Turns: []Turn{{Input: "reject this"}},
		Expect: Expectations{
			HTTPStatus:    400,
			RequireNoTask: true,
			RequireNoRun:  true,
		},
	}
	checks := EvaluateCase(c, RunSnapshot{HTTPStatus: 400})
	if !ChecksPassed(checks) {
		t.Fatalf("expected rejection without task/run should pass: %+v", checks)
	}

	checks = EvaluateCase(c, RunSnapshot{HTTPStatus: 400, TaskIDs: []string{"task_leak"}, RunIDs: []string{"run_leak"}})
	if ChecksPassed(checks) {
		t.Fatalf("rejection that created durable work must fail: %+v", checks)
	}
}
