package kernel

import (
	"fmt"
	"strings"
)

// TaskClass is intentionally coarse. It is used for telemetry and prompt
// framing, not for deciding whether ordinary natural-language input is allowed
// to reach the agent.
type TaskClass string

const (
	TaskClassSimpleAnswer   TaskClass = "simple_answer"
	TaskClassCodingExample  TaskClass = "coding_example"
	TaskClassStableAdvice   TaskClass = "stable_advice"
	TaskClassExternalLookup TaskClass = "external_lookup"
	TaskClassRepoTask       TaskClass = "repo_task"
	TaskClassDebugTask      TaskClass = "debug_task"
	TaskClassCICDTask       TaskClass = "ci_cd_task"
	TaskClassGeneralTask    TaskClass = "general_task"
)

// ToolMode describes the broad tool surface exposed to the model for one turn.
type ToolMode string

const (
	ToolModeNone       ToolMode = "none"
	ToolModeWeb        ToolMode = "web"
	ToolModeLocalRead  ToolMode = "local_read"
	ToolModeLocalWrite ToolMode = "local_write"
	ToolModeFull       ToolMode = "full"
)

// PlanPolicy controls whether update_plan is exposed and encouraged.
type PlanPolicy string

const (
	PlanPolicyDisabled PlanPolicy = "disabled"
	PlanPolicyOptional PlanPolicy = "optional"
	PlanPolicyRequired PlanPolicy = "required"
)

// WebPolicy controls web tools separately from local tools.
type WebPolicy string

const (
	WebPolicyDisabled WebPolicy = "disabled"
	WebPolicyExplicit WebPolicy = "explicit_only"
	WebPolicyEnabled  WebPolicy = "enabled"
)

// TaskStrategy is the per-turn guardrail layer between the gateway and the
// agent. A nil AllowedTools map means "allow all tools not hidden"; an empty
// map means "allow no tools".
type TaskStrategy struct {
	Class                 TaskClass
	ToolMode              ToolMode
	PlanPolicy            PlanPolicy
	WebPolicy             WebPolicy
	AllowedTools          map[string]bool
	HiddenTools           map[string]bool
	MaxIterations         int
	RequireProgressEvents bool
	ChannelMode           string
	Reason                string
}

// DefaultTaskStrategy is agent-first: expose the local tool surface and let the
// model decide whether tools are useful. Expensive or externally scoped tools
// stay hidden until the user explicitly asks for them.
func DefaultTaskStrategy() TaskStrategy {
	return TaskStrategy{
		Class:                 TaskClassGeneralTask,
		ToolMode:              ToolModeFull,
		PlanPolicy:            PlanPolicyOptional,
		WebPolicy:             WebPolicyDisabled,
		HiddenTools:           hiddenToolsFor(PlanPolicyOptional, WebPolicyDisabled),
		RequireProgressEvents: true,
		ChannelMode:           "default",
		Reason:                "agent-first default; web disabled unless explicitly requested",
	}
}

// BuildTaskStrategy returns policy guardrails for the agent. It must not become
// a natural-language task classifier. Ordinary input should reach the agent;
// this layer only limits capabilities that are clearly outside the user's
// explicit request or the current channel.
func BuildTaskStrategy(prompt, channel string) TaskStrategy {
	clean := strings.TrimSpace(prompt)
	if clean == "" {
		return TaskStrategy{
			Class:                 TaskClassSimpleAnswer,
			ToolMode:              ToolModeNone,
			PlanPolicy:            PlanPolicyDisabled,
			WebPolicy:             WebPolicyDisabled,
			AllowedTools:          map[string]bool{},
			HiddenTools:           hiddenToolsFor(PlanPolicyDisabled, WebPolicyDisabled),
			MaxIterations:         1,
			RequireProgressEvents: false,
			ChannelMode:           normalizeChannelMode(channel),
			Reason:                "empty input",
		}
	}

	policy := DefaultTaskStrategy()
	policy.ChannelMode = normalizeChannelMode(channel)
	if wantsExternalLookupText(strings.ToLower(clean)) {
		policy.Class = TaskClassExternalLookup
		policy.WebPolicy = WebPolicyEnabled
		policy.Reason = "agent-first turn with explicit external lookup"
	} else {
		policy.Class = TaskClassGeneralTask
		policy.WebPolicy = WebPolicyDisabled
		policy.Reason = "agent-first turn; web disabled unless explicitly requested"
	}
	policy.HiddenTools = hiddenToolsFor(policy.PlanPolicy, policy.WebPolicy)
	return policy
}

func normalizeChannelMode(channel string) string {
	switch strings.ToLower(strings.TrimSpace(channel)) {
	case "cli", "tui", "terminal":
		return "interactive"
	case "wechat", "weixin", "telegram", "slack", "dingtalk", "feishu":
		return "im"
	case "":
		return "default"
	default:
		return strings.ToLower(strings.TrimSpace(channel))
	}
}

func hiddenToolsFor(plan PlanPolicy, web WebPolicy) map[string]bool {
	hidden := map[string]bool{}
	if plan == PlanPolicyDisabled {
		hidden["update_plan"] = true
	}
	if web != WebPolicyEnabled {
		hidden["web_search"] = true
		hidden["web_extract"] = true
	}
	return hidden
}

func (s TaskStrategy) normalized() TaskStrategy {
	if s.Class == "" && s.ToolMode == "" && s.PlanPolicy == "" && s.WebPolicy == "" {
		return DefaultTaskStrategy()
	}
	if s.ToolMode == "" {
		s.ToolMode = ToolModeFull
	}
	if s.PlanPolicy == "" {
		s.PlanPolicy = PlanPolicyOptional
	}
	if s.WebPolicy == "" {
		s.WebPolicy = WebPolicyDisabled
	}
	if s.HiddenTools == nil {
		s.HiddenTools = hiddenToolsFor(s.PlanPolicy, s.WebPolicy)
	}
	return s
}

// AllowsTool is the final authority checked before a tool schema is exposed or
// a fallback-format tool call is executed.
func (s TaskStrategy) AllowsTool(name string) bool {
	s = s.normalized()
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	if s.HiddenTools != nil && s.HiddenTools[name] {
		return false
	}
	if s.AllowedTools != nil {
		return s.AllowedTools[name]
	}
	return true
}

func (s TaskStrategy) SystemPromptNote() string {
	s = s.normalized()
	var sb strings.Builder
	sb.WriteString("\n# AGENT-FIRST TASK POLICY\n")
	sb.WriteString(fmt.Sprintf("Task class: %s. Tool mode: %s. Planning: %s. Web: %s. Channel: %s.\n",
		s.Class, s.ToolMode, s.PlanPolicy, s.WebPolicy, s.ChannelMode))
	if s.ToolMode == ToolModeNone {
		sb.WriteString("Answer directly. Do not call tools for this turn.\n")
	} else {
		sb.WriteString("All non-command user input has been routed to the agent. Do not assume a short message is casual; use the conversation, task, workspace, and resume context to decide what to do.\n")
		sb.WriteString("When the user replies with a brief acceptance or continuation such as ok, yes, continue, proceed, or equivalent wording, inspect the previous assistant/task context and continue the proposed work if that is what the user approved.\n")
		sb.WriteString("You decide whether tools are useful. Prefer a direct answer for pure questions and small snippets when no local or external state is needed.\n")
		sb.WriteString("Use local tools when the user asks you to inspect, create, change, run, validate, or reason about files, directories, repositories, command output, workspace state, or a runnable artifact.\n")
		sb.WriteString("For ambiguous CLI requests that may produce an artifact, do a cheap read-only probe first, such as listing the current directory, then decide whether to answer inline, create a standalone file, or ask a brief clarification.\n")
	}
	if s.PlanPolicy == PlanPolicyDisabled {
		sb.WriteString("Do not call update_plan for this turn.\n")
	} else if s.PlanPolicy == PlanPolicyRequired {
		sb.WriteString("Use update_plan early, keep it current, and continue through verification or a clear blocker.\n")
	} else {
		sb.WriteString("Use update_plan only when the work genuinely needs multiple visible steps.\n")
	}
	if s.WebPolicy == WebPolicyDisabled {
		sb.WriteString("Do not use web tools unless the user explicitly asks to search, browse, inspect a URL, or retrieve current external information.\n")
	}
	if s.ChannelMode == "im" {
		sb.WriteString("For IM channels, avoid token-by-token narration; preserve concise progress milestones and final outcomes. If a write or command needs a workspace and none is clearly bound, ask the user to select or bind one before acting.\n")
	}
	return sb.String()
}

func wantsExternalLookupText(lower string) bool {
	if lower == "" {
		return false
	}
	if strings.Contains(lower, "http://") || strings.Contains(lower, "https://") {
		return true
	}
	return containsMarker(lower, []string{
		"search web", "web search", "browse", "look up", "lookup",
		"latest", "today", "news", "pricing", "price", "release notes",
		"official docs", "official documentation", "current version",
		"\u641c\u7d22", "\u8054\u7f51", "\u4e0a\u7f51", "\u6d4f\u89c8\u7f51\u9875",
		"\u67e5\u4e00\u4e0b", "\u67e5\u8be2", "\u67e5\u627e", "\u627e\u4e00\u4e0b",
		"\u5b98\u7f51", "\u5b98\u65b9\u6587\u6863", "\u6700\u65b0", "\u65b0\u95fb",
		"\u4ef7\u683c", "\u62a5\u4ef7", "\u53d1\u5e03\u8bf4\u660e", "\u5f53\u524d\u7248\u672c",
	})
}

func containsMarker(text string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
