package httpapi

import (
	"testing"

	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
)

func TestTaskStrategyForRequestUsesOriginalContent(t *testing.T) {
	req := api.MessageRequest{
		Channel: "cli",
		Content: "\u7528PHP\u5b9e\u73b0\u4e00\u4e2apgsql\u7684\u64cd\u4f5c\u793a\u4f8b",
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
	if strategy.AllowsTool("update_plan") || strategy.AllowsTool("read_file") {
		t.Fatalf("simple coding example should stay direct despite gateway task context: %+v", strategy)
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
}
