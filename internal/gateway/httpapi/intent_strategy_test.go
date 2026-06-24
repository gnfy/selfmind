package httpapi

import (
	"testing"

	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
)

func TestTaskStrategyForRequestIsAgentFirst(t *testing.T) {
	req := api.MessageRequest{
		Channel: "cli",
		Content: "write a PHP pgsql operation example",
	}
	intent := router.IntentResult{
		Intent:           router.IntentTask,
		Confidence:       0.9,
		Reason:           "test",
		ShouldCreateTask: true,
		ShouldUseTools:   true,
	}

	strategy := taskStrategyForRequest(req, intent)
	if strategy.Class != kernel.TaskClassGeneralTask {
		t.Fatalf("class = %s, want %s", strategy.Class, kernel.TaskClassGeneralTask)
	}
	if !strategy.AllowsTool("read_file") || !strategy.AllowsTool("write_file") || !strategy.AllowsTool("update_plan") {
		t.Fatalf("agent-first request should keep local tools and optional plan available: %+v", strategy)
	}
	if strategy.AllowsTool("web_search") {
		t.Fatalf("web should stay hidden without an explicit lookup request: %+v", strategy)
	}
}

func TestTaskStrategyForContinueKeepsToolsAvailable(t *testing.T) {
	req := api.MessageRequest{Channel: "weixin", Content: "\u7ee7\u7eed"}
	intent := router.IntentResult{
		Intent:           router.IntentContinue,
		Confidence:       0.9,
		Reason:           "continue",
		ShouldCreateTask: true,
		ShouldUseTools:   true,
	}

	strategy := taskStrategyForRequest(req, intent)
	if strategy.ToolMode != kernel.ToolModeFull {
		t.Fatalf("tool mode = %s, want full", strategy.ToolMode)
	}
	if !strategy.AllowsTool("update_plan") || !strategy.AllowsTool("read_file") {
		t.Fatalf("continue should expose task tools: %+v", strategy)
	}
	if strategy.AllowsTool("web_search") {
		t.Fatalf("continue should not enable web unless the user asked for it: %+v", strategy)
	}
}
