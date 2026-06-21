package httpapi

import (
	"strings"

	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
)

func taskStrategyForRequest(req api.MessageRequest, intent router.IntentResult) kernel.TaskStrategy {
	strategy := kernel.BuildTaskStrategy(req.Content, req.Channel)
	switch intent.Intent {
	case router.IntentContinue:
		strategy.ToolMode = kernel.ToolModeFull
		strategy.PlanPolicy = kernel.PlanPolicyOptional
		strategy.AllowedTools = nil
		strategy.Reason = combineReasons("continue request", intent.Reason, strategy.Reason)
	case router.IntentTask:
		if intent.ShouldUseTools {
			strategy.Reason = combineReasons("task intent", intent.Reason, strategy.Reason)
		}
	case router.IntentCasual:
		if !intent.ShouldUseTools {
			strategy.ToolMode = kernel.ToolModeNone
			strategy.PlanPolicy = kernel.PlanPolicyDisabled
			strategy.WebPolicy = kernel.WebPolicyDisabled
			strategy.AllowedTools = map[string]bool{}
			strategy.MaxIterations = 1
			strategy.RequireProgressEvents = false
			strategy.Reason = combineReasons("casual intent", intent.Reason, strategy.Reason)
		}
	}
	return strategy
}

func combineReasons(parts ...string) string {
	var out []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return strings.Join(out, "; ")
}
