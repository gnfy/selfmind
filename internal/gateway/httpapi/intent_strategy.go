package httpapi

import (
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/tools"
)

func taskStrategyForRequest(req api.MessageRequest, intent router.IntentResult) kernel.TaskStrategy {
	strategy := kernel.BuildTaskStrategy(req.Content, req.Channel)
	// The model must KNOW the execution environment (sandbox + network policy)
	// instead of discovering it through opaque failures — a network-less
	// sandbox otherwise burns whole turns on blind retries that look like
	// timeouts. Rendered only for tool-capable turns (SystemPromptNote).
	strategy.ExecSandboxNote = tools.ExecSandboxPromptNote()
	if req.AllowWeb {
		strategy = strategy.WithWebEnabled()
	}
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
		strategy.Reason = combineReasons("agent-first normal input", intent.Reason, strategy.Reason)
	}
	if strings.TrimSpace(req.RecoveryMode) == control.RunRecoveryModeVerifyOnly {
		strategy.VerificationOnly = true
		strategy.ToolMode = kernel.ToolModeLocalRead
		strategy.PlanPolicy = kernel.PlanPolicyOptional
		strategy.Reason = combineReasons("verification-only automatic recovery", strategy.Reason)
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
