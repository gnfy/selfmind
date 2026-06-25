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
	if strategy.Class != kernel.TaskClassCodingExample {
		t.Fatalf("class = %s, want %s", strategy.Class, kernel.TaskClassCodingExample)
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

func TestTaskStrategyForIdentityQuestionDisablesActionTools(t *testing.T) {
	req := api.MessageRequest{
		Channel: "cli",
		Content: "\u4f60\u662f\u8c01\uff1f\u5f53\u524d\u8fde\u63a5\u7684\u6a21\u578b\u662f\u4ec0\u4e48\uff1f",
	}
	intent := router.IntentResult{
		Intent:           router.IntentTask,
		Confidence:       0.9,
		Reason:           "agent-first default",
		ShouldCreateTask: true,
		ShouldUseTools:   true,
	}

	strategy := taskStrategyForRequest(req, intent)
	if strategy.Class != kernel.TaskClassSimpleAnswer {
		t.Fatalf("class = %s, want simple_answer", strategy.Class)
	}
	if strategy.ToolMode != kernel.ToolModeNone {
		t.Fatalf("tool mode = %s, want none", strategy.ToolMode)
	}
	if strategy.AllowsTool("read_file") || strategy.AllowsTool("terminal") || strategy.AllowsTool("update_plan") {
		t.Fatalf("identity question should not expose action tools: %+v", strategy)
	}
}
