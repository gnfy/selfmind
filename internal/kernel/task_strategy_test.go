package kernel

import (
	"testing"

	"selfmind/internal/kernel/llm"
)

func TestTaskStrategyCodingExampleUsesNoTools(t *testing.T) {
	strategy := BuildTaskStrategy("\u7528PHP\u5b9e\u73b0\u4e00\u4e2apgsql\u7684\u64cd\u4f5c\u793a\u4f8b", "cli")

	if strategy.Class != TaskClassCodingExample {
		t.Fatalf("class = %s, want %s", strategy.Class, TaskClassCodingExample)
	}
	if strategy.ToolMode != ToolModeNone {
		t.Fatalf("tool mode = %s, want %s", strategy.ToolMode, ToolModeNone)
	}
	if strategy.AllowsTool("read_file") || strategy.AllowsTool("update_plan") || strategy.AllowsTool("web_search") {
		t.Fatalf("coding example should expose no tools: %+v", strategy)
	}
	if strategy.MaxIterations != 1 {
		t.Fatalf("max iterations = %d, want 1", strategy.MaxIterations)
	}
}

func TestTaskStrategyExplicitLookupAllowsWebOnly(t *testing.T) {
	strategy := BuildTaskStrategy("\u67e5\u4e00\u4e0b Go \u5f53\u524d\u6700\u65b0\u7a33\u5b9a\u7248\u672c", "cli")

	if strategy.Class != TaskClassExternalLookup {
		t.Fatalf("class = %s, want %s", strategy.Class, TaskClassExternalLookup)
	}
	if !strategy.AllowsTool("web_search") || !strategy.AllowsTool("web_extract") {
		t.Fatalf("explicit lookup should expose web tools: %+v", strategy)
	}
	if strategy.AllowsTool("read_file") {
		t.Fatalf("simple external lookup should not expose local file tools: %+v", strategy)
	}
	if strategy.AllowsTool("update_plan") {
		t.Fatalf("simple external lookup should not expose update_plan: %+v", strategy)
	}
}

func TestTaskStrategyRepoTaskAllowsLocalToolsButSuppressesWeb(t *testing.T) {
	strategy := BuildTaskStrategy("check this local repo, inspect README and run tests", "cli")

	if strategy.Class != TaskClassRepoTask {
		t.Fatalf("class = %s, want %s", strategy.Class, TaskClassRepoTask)
	}
	if !strategy.AllowsTool("read_file") || !strategy.AllowsTool("terminal") {
		t.Fatalf("repo task should expose local tools: %+v", strategy)
	}
	if strategy.AllowsTool("web_search") || strategy.AllowsTool("web_extract") {
		t.Fatalf("repo task should suppress web tools by default: %+v", strategy)
	}
	if !strategy.AllowsTool("update_plan") {
		t.Fatalf("repo task should allow optional planning: %+v", strategy)
	}
}

func TestTaskStrategyDebugTaskRequiresPlan(t *testing.T) {
	strategy := BuildTaskStrategy("fix the failing tests and explain the regression", "cli")

	if strategy.Class != TaskClassDebugTask {
		t.Fatalf("class = %s, want %s", strategy.Class, TaskClassDebugTask)
	}
	if strategy.PlanPolicy != PlanPolicyRequired {
		t.Fatalf("plan policy = %s, want %s", strategy.PlanPolicy, PlanPolicyRequired)
	}
	if !strategy.AllowsTool("update_plan") {
		t.Fatalf("debug task should expose update_plan: %+v", strategy)
	}
}

func TestFilterToolCallsByStrategyBlocksHiddenFallbackCalls(t *testing.T) {
	strategy := BuildTaskStrategy("\u7528Go\u5199\u4e00\u4e2a\u4e8c\u5206\u6cd5", "cli")
	calls := []llm.ToolCall{
		{Function: "read_file"},
		{Function: "update_plan"},
	}

	if got := filterToolCallsByStrategy(calls, strategy); len(got) != 0 {
		t.Fatalf("hidden fallback calls were not filtered: %+v", got)
	}
}
