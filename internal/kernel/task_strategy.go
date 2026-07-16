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

// MayWriteWorkspace reports whether this turn's tool surface can mutate the
// workspace (create/edit files, run write-capable commands). Only write-capable
// turns need per-workspace serialization; read-only turns (no tools, web-only,
// or local-read) are safe to run concurrently on the same workspace. This is the
// SelfMind equivalent of codex's Exclusive-vs-SharedRead access mode.
func (m ToolMode) MayWriteWorkspace() bool {
	switch m {
	case ToolModeLocalWrite, ToolModeFull:
		return true
	default:
		return false
	}
}

// MayWriteWorkspace reports whether the turn's strategy can write the workspace.
func (s TaskStrategy) MayWriteWorkspace() bool {
	return s.ToolMode.MayWriteWorkspace()
}

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
	MaxActionTools        int
	ActionToolBudgetStep  int
	ActionToolBudgetLimit int
	MaxBudgetExtensions   int
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
		MaxActionTools:        10,
		ActionToolBudgetStep:  4,
		ActionToolBudgetLimit: 22,
		MaxBudgetExtensions:   3,
		RequireProgressEvents: true,
		ChannelMode:           "default",
		Reason:                "agent-first default; web disabled unless explicitly requested",
	}
}

// BuildTaskStrategy returns policy guardrails for the agent. Following the
// codex philosophy, it must NOT decide tool exposure by classifying natural
// language: the config-allowed tool surface stays available on every turn and
// the model decides whether a tool is useful. This layer only sets soft hints
// (task class for telemetry/fast-model routing, plan policy) and the genuine
// safety gate (web tools stay off unless the user explicitly asks). Approval,
// sandbox, and workspace scope remain enforced in middleware, not here.
func BuildTaskStrategy(prompt, channel string) TaskStrategy {
	clean := strings.TrimSpace(prompt)
	policy := DefaultTaskStrategy()
	policy.ChannelMode = normalizeChannelMode(channel)
	if clean == "" {
		policy.Reason = "empty input; agent-first default"
		return policy
	}

	lower := strings.ToLower(clean)
	switch {
	case looksLikePureDirectAnswer(clean, lower):
		// Soft hint only: route trivial identity/model questions to the fast
		// model and skip planning. Tools STAY exposed — if the guess is wrong
		// and the turn actually needs a tool, the model can still call it.
		policy.Class = TaskClassSimpleAnswer
		policy.PlanPolicy = PlanPolicyDisabled
		policy.Reason = "likely a direct-answer turn; tools stay available, model answers directly when no tool is needed"
	case wantsExternalLookupText(lower):
		policy.Class = TaskClassExternalLookup
		policy.WebPolicy = WebPolicyEnabled
		policy.Reason = "agent-first turn with explicit external lookup"
	case looksLikeCodingExample(lower):
		policy.Class = TaskClassCodingExample
		policy.Reason = "agent-first coding example; prefer a direct answer unless workspace state is needed"
	default:
		policy.Class = TaskClassGeneralTask
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
	if s.MaxActionTools <= 0 && s.ToolMode != ToolModeNone {
		s.MaxActionTools = defaultActionToolBudget(s.Class)
	}
	if s.ActionToolBudgetStep <= 0 {
		s.ActionToolBudgetStep = 4
	}
	if s.ActionToolBudgetLimit < s.MaxActionTools {
		s.ActionToolBudgetLimit = max(s.MaxActionTools, 22)
	}
	if s.MaxBudgetExtensions <= 0 {
		s.MaxBudgetExtensions = 3
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

// WithWebEnabled opts the turn into web tools and un-hides them. Used when a
// caller (e.g. a scheduled job with web=true) explicitly grants web access for
// a turn that the default policy would otherwise keep offline.
func (s TaskStrategy) WithWebEnabled() TaskStrategy {
	s = s.normalized()
	s.WebPolicy = WebPolicyEnabled
	hidden := make(map[string]bool, len(s.HiddenTools))
	for name, v := range s.HiddenTools {
		hidden[name] = v
	}
	delete(hidden, "web_search")
	delete(hidden, "web_extract")
	s.HiddenTools = hidden
	return s
}

func (s TaskStrategy) WithActionToolsDisabled() TaskStrategy {
	s = s.normalized()
	if s.AllowedTools == nil {
		s.AllowedTools = map[string]bool{}
	}
	for _, name := range lifecycleToolNames() {
		if !s.HiddenTools[name] {
			s.AllowedTools[name] = true
		}
	}
	s.ToolMode = ToolModeLocalRead
	s.MaxActionTools = 0
	return s
}

func (s TaskStrategy) WithHiddenTools(names ...string) TaskStrategy {
	s = s.normalized()
	hidden := make(map[string]bool, len(s.HiddenTools)+len(names))
	for name, value := range s.HiddenTools {
		hidden[name] = value
	}
	allowed := s.AllowedTools
	if allowed != nil {
		allowed = make(map[string]bool, len(s.AllowedTools))
		for name, value := range s.AllowedTools {
			allowed[name] = value
		}
	}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		hidden[name] = true
		if allowed != nil {
			delete(allowed, name)
		}
	}
	s.HiddenTools = hidden
	s.AllowedTools = allowed
	return s
}

func defaultActionToolBudget(class TaskClass) int {
	switch class {
	case TaskClassCodingExample:
		return 6
	case TaskClassExternalLookup:
		return 8
	case TaskClassRepoTask, TaskClassDebugTask, TaskClassCICDTask:
		return 12
	default:
		return 10
	}
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
		if s.ToolMode.MayWriteWorkspace() {
			// Inspect-before-build: the workspace is the shared source of truth
			// across endpoints, so reuse existing code instead of reinventing it.
			sb.WriteString("Before writing new code or creating a file, search the workspace for an existing implementation (grep or list/read files) and extend or reuse it instead of reinventing; the workspace is the shared source of truth.\n")
		}
		if s.MaxActionTools > 0 {
			sb.WriteString(fmt.Sprintf("Keep tool use economical. This turn starts with about %d non-lifecycle tool call(s). SelfMind may extend that budget when completed tools produce new evidence, but it will never exceed %d. update_plan and finish_run are lifecycle tools with their own small per-turn caps; do not call them repeatedly.\n", s.MaxActionTools, s.ActionToolBudgetLimit))
		}
	}
	if s.PlanPolicy == PlanPolicyDisabled {
		sb.WriteString("Do not call update_plan for this turn.\n")
	} else if s.PlanPolicy == PlanPolicyRequired {
		sb.WriteString("Use update_plan early and keep it current after meaningful step transitions. Every call must send the complete current plan snapshot, including unchanged and completed steps. Before finish_run status done, update the plan so every step is completed or cancelled. Continue through verification or a clear blocker.\n")
	} else {
		sb.WriteString("Use update_plan only when the work genuinely needs multiple visible steps. Every call must send the complete current plan snapshot, including unchanged and completed steps. Update it after meaningful transitions, and resolve every step before finish_run status done; do not update repeatedly without a real status change.\n")
	}
	if s.WebPolicy == WebPolicyDisabled {
		sb.WriteString("Do not use web tools unless the user explicitly asks to search, browse, inspect a URL, or retrieve current external information.\n")
	}
	if s.ChannelMode == "im" {
		sb.WriteString("For IM channels, avoid token-by-token narration; preserve concise progress milestones and final outcomes. If a write or command needs a workspace and none is clearly bound, ask the user to select or bind one before acting.\n")
	}
	sb.WriteString("Call finish_run only after non-trivial tool-using work that needs a durable task outcome. Skip finish_run for direct answers, small code snippets, ordinary explanations, or when you can answer clearly in one message.\n")
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

func looksLikePureDirectAnswer(clean, lower string) bool {
	if clean == "" {
		return false
	}
	runes := []rune(clean)
	if len(runes) > 80 {
		return false
	}
	if containsMarker(lower, []string{
		"who are you", "what are you", "what model", "which model",
		"your model", "current model", "configured model",
		"\u4f60\u662f\u8c01", "\u4f60\u662f\u4ec0\u4e48", "\u4ec0\u4e48\u6a21\u578b",
		"\u54ea\u4e2a\u6a21\u578b", "\u5f53\u524d\u6a21\u578b", "\u8fde\u63a5\u7684\u6a21\u578b",
		"\u4f60\u7528\u7684\u662f\u4ec0\u4e48\u6a21\u578b", "\u4f60\u662f\u4ec0\u4e48\u5927\u6a21\u578b",
	}) {
		return !containsMarker(lower, []string{
			"\u4ee3\u7801", "code", "\u6587\u4ef6", "file", "\u9879\u76ee", "repo",
			"\u4ed3\u5e93", "\u76ee\u5f55", "\u5b9e\u73b0", "create", "write",
			"\u751f\u6210", "\u5206\u6790", "inspect", "run",
		})
	}
	return false
}

func looksLikeCodingExample(lower string) bool {
	if lower == "" {
		return false
	}
	if !containsMarker(lower, []string{
		"go", "golang", "rust", "php", "python", "javascript", "js", "typescript", "ts",
		"java", "c++", "pgsql", "postgres", "sql",
		"\u4e8c\u5206", "\u793a\u4f8b", "\u4f8b\u5b50", "\u4ee3\u7801",
		"\u51fd\u6570", "\u5b9e\u73b0", "\u5199\u4e00\u4e2a",
		"example", "snippet", "function", "implement",
	}) {
		return false
	}
	return !containsMarker(lower, []string{
		"\u5f53\u524d\u9879\u76ee", "\u5f53\u524d\u76ee\u5f55", "\u8fd9\u4e2a\u9879\u76ee",
		"\u8fd9\u4e2a\u4ed3\u5e93", "\u8fd9\u4e2a\u4ee3\u7801", "\u4fee\u6539",
		"\u521b\u5efa\u6587\u4ef6", "\u4fdd\u5b58", "\u8fd0\u884c", "\u6d4b\u8bd5",
		"workspace", "repo", "codebase", "change file",
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
