package kernel

import (
	"testing"

	"selfmind/internal/kernel/llm"
)

func TestTaskStrategyDefaultIsAgentFirst(t *testing.T) {
	strategy := BuildTaskStrategy("analyze this repository and suggest improvements", "cli")

	if strategy.Class != TaskClassGeneralTask {
		t.Fatalf("class = %s, want %s", strategy.Class, TaskClassGeneralTask)
	}
	if strategy.ToolMode != ToolModeFull {
		t.Fatalf("tool mode = %s, want %s", strategy.ToolMode, ToolModeFull)
	}
	if strategy.PlanPolicy != PlanPolicyOptional {
		t.Fatalf("plan policy = %s, want %s", strategy.PlanPolicy, PlanPolicyOptional)
	}
	if !strategy.AllowsTool("read_file") || !strategy.AllowsTool("write_file") || !strategy.AllowsTool("terminal") {
		t.Fatalf("agent-first policy should leave local tools available: %+v", strategy)
	}
	if !strategy.AllowsTool("update_plan") {
		t.Fatalf("agent-first policy should leave optional planning available: %+v", strategy)
	}
	if strategy.AllowsTool("web_search") || strategy.AllowsTool("web_extract") {
		t.Fatalf("web tools should stay hidden until explicitly requested: %+v", strategy)
	}
}

func TestTaskStrategyPureModelQuestionIsSoftHintNotToolHiding(t *testing.T) {
	// Codex philosophy: a likely direct-answer turn is a SOFT hint (fast-model
	// routing + no plan), not a reason to strip the tool surface. Local tools
	// stay available so a misclassified "needs a tool" turn is not crippled.
	strategy := BuildTaskStrategy("\u4f60\u662f\u4ec0\u4e48\u6a21\u578b\uff1f", "cli")

	if strategy.Class != TaskClassSimpleAnswer {
		t.Fatalf("class = %s, want %s", strategy.Class, TaskClassSimpleAnswer)
	}
	if strategy.ToolMode != ToolModeFull {
		t.Fatalf("tool mode = %s, want %s (tools stay exposed)", strategy.ToolMode, ToolModeFull)
	}
	if strategy.PlanPolicy != PlanPolicyDisabled {
		t.Fatalf("plan policy = %s, want %s for a trivial turn", strategy.PlanPolicy, PlanPolicyDisabled)
	}
	if !strategy.AllowsTool("read_file") || !strategy.AllowsTool("terminal") {
		t.Fatalf("direct-answer hint must not strip local tools: %+v", strategy)
	}
	if strategy.AllowsTool("update_plan") {
		t.Fatalf("planning should be hidden when plan policy is disabled: %+v", strategy)
	}
	if strategy.AllowsTool("web_search") {
		t.Fatalf("web tools should stay hidden until explicitly requested: %+v", strategy)
	}
}

func TestTaskStrategyCodingExampleKeepsAgentFirstToolsOptional(t *testing.T) {
	strategy := BuildTaskStrategy("\u7528 PHP \u5b9e\u73b0\u4e00\u4e2a pgsql \u64cd\u4f5c\u793a\u4f8b", "cli")

	if strategy.Class != TaskClassCodingExample {
		t.Fatalf("class = %s, want %s", strategy.Class, TaskClassCodingExample)
	}
	if !strategy.AllowsTool("read_file") || !strategy.AllowsTool("write_file") || !strategy.AllowsTool("update_plan") {
		t.Fatalf("coding examples should keep agent-first optional tools available: %+v", strategy)
	}
}

func TestTaskStrategyExplicitLookupEnablesWebWithoutHidingLocalTools(t *testing.T) {
	strategy := BuildTaskStrategy("look up the latest stable Go version", "cli")

	if strategy.Class != TaskClassExternalLookup {
		t.Fatalf("class = %s, want %s", strategy.Class, TaskClassExternalLookup)
	}
	if !strategy.AllowsTool("web_search") || !strategy.AllowsTool("web_extract") {
		t.Fatalf("explicit lookup should expose web tools: %+v", strategy)
	}
	if !strategy.AllowsTool("read_file") || !strategy.AllowsTool("terminal") {
		t.Fatalf("explicit lookup should not remove local tools from the agent: %+v", strategy)
	}
	if !strategy.AllowsTool("update_plan") {
		t.Fatalf("explicit lookup should keep optional planning available: %+v", strategy)
	}
}

func TestTaskStrategyEmptyInputFallsBackToAgentFirstDefault(t *testing.T) {
	// Empty input is degenerate (the gateway guards it), but it must not invent
	// a tool-hiding classification. Fall back to the agent-first default.
	strategy := BuildTaskStrategy("   ", "cli")

	if strategy.ToolMode != ToolModeFull {
		t.Fatalf("tool mode = %s, want %s", strategy.ToolMode, ToolModeFull)
	}
	if !strategy.AllowsTool("read_file") {
		t.Fatalf("empty input should still fall back to the default tool surface: %+v", strategy)
	}
	if strategy.AllowsTool("web_search") {
		t.Fatalf("web tools should stay hidden by default: %+v", strategy)
	}
}

func TestFilterToolCallsByStrategyOnlyBlocksHiddenWebCalls(t *testing.T) {
	strategy := BuildTaskStrategy("write a PHP pgsql operation example", "cli")
	calls := []llm.ToolCall{
		{Function: "read_file"},
		{Function: "update_plan"},
		{Function: "web_search"},
	}

	got := filterToolCallsByStrategy(calls, strategy)
	if len(got) != 2 || got[0].Function != "read_file" || got[1].Function != "update_plan" {
		t.Fatalf("local and planning tools should pass while hidden web calls are filtered: %+v", got)
	}
}

func TestFilterToolCallsByLifecycleCaps(t *testing.T) {
	calls := []llm.ToolCall{
		{Function: "update_plan"},
		{Function: "finish_run"},
		{Function: "read_file"},
	}
	got, dropped := filterToolCallsByLifecycleCaps(calls, map[string]int{
		"update_plan": 16,
		"finish_run":  1,
	})
	if dropped != 2 {
		t.Fatalf("dropped = %d, want 2", dropped)
	}
	if len(got) != 1 || got[0].Function != "read_file" {
		t.Fatalf("got = %+v, want only read_file", got)
	}
}

func TestFilterToolCallsByLifecycleCapsConsumesCurrentBatch(t *testing.T) {
	calls := []llm.ToolCall{
		{Function: "update_plan"},
		{Function: "update_plan"},
		{Function: "finish_run"},
		{Function: "finish_run"},
		{Function: "read_file"},
	}
	got, dropped := filterToolCallsByLifecycleCaps(calls, map[string]int{
		"update_plan": 15,
	})
	if dropped != 2 {
		t.Fatalf("dropped = %d, want 2", dropped)
	}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3: %+v", len(got), got)
	}
	if got[0].Function != "update_plan" || got[1].Function != "finish_run" || got[2].Function != "read_file" {
		t.Fatalf("got = %+v, want one update_plan, one finish_run, and read_file", got)
	}
}

func TestUpdatePlanLifecycleCapSupportsMeaningfulProgress(t *testing.T) {
	if got := lifecycleToolCap("update_plan"); got != 16 {
		t.Fatalf("update_plan lifecycle cap = %d, want 16", got)
	}
	calls := make([]llm.ToolCall, 17)
	for i := range calls {
		calls[i] = llm.ToolCall{Function: "update_plan"}
	}
	got, dropped := filterToolCallsByLifecycleCaps(calls, map[string]int{})
	if len(got) != 16 || dropped != 1 {
		t.Fatalf("len(got)=%d dropped=%d, want 16 and 1", len(got), dropped)
	}
}

func TestUnresolvedPlanStepsFromToolCall(t *testing.T) {
	call := llm.ToolCall{Function: "update_plan", Args: `{"plan":[{"step":"inspect","status":"completed"},{"step":"verify","status":"in_progress"},{"step":"optional cleanup","status":"cancelled"}]}`}
	got, ok := unresolvedPlanStepsFromToolCall(call)
	if !ok || len(got) != 1 || got[0] != "verify" {
		t.Fatalf("unresolved plan = %v, ok=%v; want [verify], true", got, ok)
	}
}

func TestFinishRunStatusFromToolCall(t *testing.T) {
	got, ok := finishRunStatusFromToolCall(llm.ToolCall{Function: "finish_run", Args: `{"status":"waiting_external"}`})
	if !ok || got != "waiting_external" {
		t.Fatalf("finish status = %q, ok=%v", got, ok)
	}
}

func TestWithWebEnabledUnhidesWebTools(t *testing.T) {
	// Default strategy keeps web tools hidden; WithWebEnabled must un-hide them
	// and flip the policy, without touching local tools.
	s := DefaultTaskStrategy()
	if s.AllowsTool("web_search") {
		t.Fatal("precondition: web should be hidden by default")
	}
	s = s.WithWebEnabled()
	if !s.AllowsTool("web_search") || !s.AllowsTool("web_extract") {
		t.Fatalf("web tools should be available after WithWebEnabled: %+v", s)
	}
	if s.WebPolicy != WebPolicyEnabled {
		t.Fatalf("web policy = %s, want enabled", s.WebPolicy)
	}
	if !s.AllowsTool("read_file") {
		t.Fatalf("local tools must remain available: %+v", s)
	}
}
