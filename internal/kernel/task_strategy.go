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

// MayWriteWorkspace is the scheduler's conservative capability check. ToolMode
// is a broad prompt hint, not a closed tool list: a local_read hint with nil
// AllowedTools still exposes write_file and terminal. Only an enforced
// no-action mode or an explicit lifecycle-only allowlist proves otherwise.
func (s TaskStrategy) MayWriteWorkspace() bool {
	if s.ToolMode == ToolModeNone {
		return false
	}
	if s.AllowedTools == nil {
		return true
	}
	for name, allowed := range s.AllowedTools {
		if allowed && !isLifecycleToolName(name) {
			return true
		}
	}
	return false
}

// PlanPolicy controls whether update_plan is exposed and encouraged.
type PlanPolicy string

const (
	PlanPolicyDisabled PlanPolicy = "disabled"
	PlanPolicyOptional PlanPolicy = "optional"
	PlanPolicyRequired PlanPolicy = "required"
)

// planStaleReminderThreshold is how many tool actions a Run may perform after
// its last plan update, with steps still open, before the next model call is
// reminded that the visible plan may be out of date. Plan updates stay the
// model's; one run went 58 tool calls without one and showed its first step
// in progress for seven minutes.
const planStaleReminderThreshold = 12

// shouldRemindStalePlan decides whether the visible plan is stale enough to
// remind the model: steps are still open and enough actions happened since
// the plan was last updated, or since the last reminder.
func shouldRemindStalePlan(strategy TaskStrategy, openSteps, actionsSinceUpdate, actionsAtReminder int) bool {
	if openSteps == 0 || !strategy.normalized().AllowsTool("update_plan") {
		return false
	}
	return actionsSinceUpdate-actionsAtReminder >= planStaleReminderThreshold
}

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
	// CompletionReserve is installed from actual tool availability, never
	// request keywords. It keeps part of the existing hard ceiling for checks.
	CompletionReserve     int
	RequireProgressEvents bool
	ChannelMode           string
	Reason                string
	// ExecSandboxNote is a gateway-supplied one-liner describing the effective
	// command-execution environment (sandbox on/off, network policy). Rendered
	// into the system prompt for tool-capable turns so the model can diagnose
	// sandbox-caused failures instead of blind-retrying. Empty = no note.
	// Populated by the gateway (tools.ExecSandboxPromptNote); kernel stays free
	// of tool-package dependencies.
	ExecSandboxNote string
	// VerificationOnly exposes observation and lifecycle tools only. It is a
	// trusted recovery contract, never a natural-language classification.
	VerificationOnly bool
}

// ToolBudgetPolicy is the configurable envelope around the evidence-gated
// action-tool budget. It deliberately contains no language, framework, tool,
// or task-name rules.
type ToolBudgetPolicy struct {
	Initial       int
	Step          int
	Limit         int
	MaxExtensions int
}

func (p ToolBudgetPolicy) normalized() ToolBudgetPolicy {
	if p.Initial < 0 {
		p.Initial = 0
	}
	if p.Step < 0 {
		p.Step = 0
	}
	if p.Limit < 0 {
		p.Limit = 0
	}
	if p.MaxExtensions < 0 {
		p.MaxExtensions = 0
	}
	if p.Limit > 0 && p.Initial > p.Limit {
		p.Initial = p.Limit
	}
	return p
}

func (p ToolBudgetPolicy) apply(strategy TaskStrategy) TaskStrategy {
	p = p.normalized()
	if strategy.ToolMode == ToolModeNone {
		return strategy
	}
	if p.Initial > 0 {
		strategy.MaxActionTools = p.Initial
	}
	if p.Step > 0 {
		strategy.ActionToolBudgetStep = p.Step
	}
	if p.Limit > 0 {
		strategy.ActionToolBudgetLimit = p.Limit
	}
	if p.MaxExtensions > 0 {
		strategy.MaxBudgetExtensions = p.MaxExtensions
	}
	if strategy.ChannelMode == "im" && strategy.Class == TaskClassExternalLookup {
		if strategy.ActionToolBudgetLimit > 20 {
			strategy.ActionToolBudgetLimit = 20
		}
		if strategy.MaxBudgetExtensions > 2 {
			strategy.MaxBudgetExtensions = 2
		}
	}
	return strategy.normalized()
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
		ActionToolBudgetLimit: 34,
		MaxBudgetExtensions:   6,
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

// AdmittedTaskStrategy rebuilds the strategy a Run was admitted with from the
// identity its admission froze. The hidden tool set follows from the plan and
// web policies and budgets start from the defaults, exactly as for a strategy
// built from the request; prompt framing derived from the current
// environment, such as the sandbox note, is the caller's to add.
func AdmittedTaskStrategy(class TaskClass, toolMode ToolMode, plan PlanPolicy, web WebPolicy, channelMode string, verificationOnly bool) TaskStrategy {
	strategy := DefaultTaskStrategy()
	strategy.Class, strategy.ToolMode, strategy.PlanPolicy, strategy.WebPolicy = class, toolMode, plan, web
	strategy.ChannelMode, strategy.VerificationOnly = channelMode, verificationOnly
	strategy.HiddenTools = hiddenToolsFor(plan, web)
	strategy.Reason = "the strategy this work was admitted with"
	return strategy
}

func normalizeChannelMode(channel string) string {
	lower := strings.ToLower(strings.TrimSpace(channel))
	switch {
	case lower == "cli" || lower == "tui" || lower == "terminal":
		return "interactive"
	case lower == "wechat" || lower == "weixin" || lower == "telegram" || lower == "slack" || lower == "dingtalk" || lower == "feishu" || lower == "qq" ||
		strings.HasSuffix(lower, "@im.wechat") || strings.Contains(lower, "@im."):
		return "im"
	case lower == "":
		return "default"
	default:
		return lower
	}
}

func promptChannelLabel(mode string) string {
	if looksLikeUUID(mode) {
		return "interactive"
	}
	return mode
}

func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
	}
	return true
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
		s.ActionToolBudgetLimit = max(s.MaxActionTools, 34)
	}
	if s.MaxBudgetExtensions <= 0 {
		s.MaxBudgetExtensions = 6
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
	s.AllowedTools = map[string]bool{}
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
		s.Class, s.ToolMode, s.PlanPolicy, s.WebPolicy, promptChannelLabel(s.ChannelMode)))
	if s.ToolMode == ToolModeNone {
		sb.WriteString("Answer directly. Do not call tools for this turn.\n")
	} else {
		if s.VerificationOnly {
			sb.WriteString("This is a verification-only recovery turn. Observe current state and resolve uncertainty; do not mutate files, processes, services, or external systems. If read-only evidence cannot determine the result, finish with an actionable blocker.\n")
		}
		sb.WriteString("All non-command user input has been routed to the agent. Do not assume a short message is casual; use the conversation, task, workspace, and resume context to decide what to do.\n")
		sb.WriteString("Interpret the user's reply in context, including prior proposals and work history. Proceed with clearly accepted work within its authorized scope.\n")
		sb.WriteString("You decide whether tools are useful. Prefer a direct answer for pure questions and small snippets when no local or external state is needed.\n")
		sb.WriteString("Use local tools when the user asks you to inspect, create, change, run, validate, or reason about files, directories, repositories, command output, workspace state, or a runnable artifact.\n")
		sb.WriteString("For ambiguous CLI requests that may produce an artifact, do a cheap read-only probe first, such as listing the current directory, then decide whether to answer inline, create a standalone file, or ask a brief clarification.\n")
		sb.WriteString("Bound external CLI and API queries with narrow filters, explicit limits, or short timeouts instead of wide cluster or history dumps. Do not re-run a timed-out wide command without narrowing its scope.\n")
		sb.WriteString("For read-only status or inspection requests, once authoritative workspace records and a bounded live check answer the question, report the result directly instead of exhaustively auditing unrelated history.\n")
		sb.WriteString("Cross-channel delivery and gateway notifications use registered runtime tools (discoverable via tool_search, such as set_delivery_target), never by searching user repositories for channel scripts.\n")
		if s.ToolMode.MayWriteWorkspace() {
			// Inspect-before-build: the workspace is the shared source of truth
			// across endpoints, so reuse existing code instead of reinventing it.
			sb.WriteString("Before writing new code or creating a file, search the workspace for an existing implementation (grep or list/read files) and extend or reuse it instead of reinventing; the workspace is the shared source of truth.\n")
		}
		if s.ExecSandboxNote != "" {
			sb.WriteString(s.ExecSandboxNote)
		}
		if s.MaxActionTools > 0 {
			sb.WriteString(fmt.Sprintf("Keep tool use economical. This turn starts with about %d action tool call(s). SelfMind may extend that budget when completed tools produce new evidence, but it will never exceed %d. Do not repeat lifecycle or action calls without a real state change.\n", s.MaxActionTools, s.ActionToolBudgetLimit))
		}
	}
	if s.WebPolicy == WebPolicyDisabled {
		sb.WriteString("Do not use web tools unless the user explicitly asks to search, browse, inspect a URL, or retrieve current external information.\n")
	}
	if s.ChannelMode == "im" {
		sb.WriteString("For IM channels, avoid token-by-token narration; preserve concise progress milestones and final outcomes. Keep mobile replies scannable: summarize multi-item inspections in a compact overview instead of repeating per-item command blocks or raw JSON unless asked, and report only observed values (omit unqueried timestamps/fields rather than emitting placeholders like `16:2xZ`). If a write or command needs a workspace and none is clearly bound, ask the user to select or bind one before acting.\n")
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
