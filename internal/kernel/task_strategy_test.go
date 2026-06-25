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

func TestTaskStrategyPureModelQuestionUsesDirectAnswer(t *testing.T) {
	strategy := BuildTaskStrategy("\u4f60\u662f\u4ec0\u4e48\u6a21\u578b\uff1f", "cli")

	if strategy.Class != TaskClassSimpleAnswer {
		t.Fatalf("class = %s, want %s", strategy.Class, TaskClassSimpleAnswer)
	}
	if strategy.ToolMode != ToolModeNone {
		t.Fatalf("tool mode = %s, want none", strategy.ToolMode)
	}
	if strategy.AllowsTool("read_file") || strategy.AllowsTool("update_plan") || strategy.AllowsTool("terminal") {
		t.Fatalf("pure direct answer should expose no tools: %+v", strategy)
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

func TestTaskStrategyEmptyInputHasNoTools(t *testing.T) {
	strategy := BuildTaskStrategy("   ", "cli")

	if strategy.ToolMode != ToolModeNone {
		t.Fatalf("tool mode = %s, want none", strategy.ToolMode)
	}
	if strategy.AllowsTool("read_file") || strategy.AllowsTool("update_plan") || strategy.AllowsTool("web_search") {
		t.Fatalf("empty input should expose no tools: %+v", strategy)
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
