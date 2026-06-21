package kernel

import (
	"fmt"
	"strings"
)

// TaskClass is the agent's coarse understanding of the current user request.
// It is intentionally small and channel-independent so CLI, IM, and future web
// clients can share the same tool policy.
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

// TaskStrategy is the per-turn policy layer between user intent and tool
// exposure. A nil AllowedTools map means "allow all tools not hidden"; an empty
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

// DefaultTaskStrategy preserves the legacy broad tool surface for internal
// callers that do not have a concrete user prompt.
func DefaultTaskStrategy() TaskStrategy {
	return TaskStrategy{
		Class:                 TaskClassGeneralTask,
		ToolMode:              ToolModeFull,
		PlanPolicy:            PlanPolicyOptional,
		WebPolicy:             WebPolicyEnabled,
		RequireProgressEvents: true,
		ChannelMode:           "default",
		Reason:                "default broad tool policy",
	}
}

// BuildTaskStrategy classifies a single user request and returns the tool
// policy SelfMind should expose to the model for that turn.
func BuildTaskStrategy(prompt, channel string) TaskStrategy {
	clean := strings.TrimSpace(prompt)
	lower := strings.ToLower(clean)
	explicitWeb := wantsExternalLookupText(lower)

	if looksLikeCICDTask(lower) {
		return complexStrategy(TaskClassCICDTask, ToolModeFull, PlanPolicyRequired, explicitWeb, channel, "CI/CD or deployment task")
	}
	if looksLikeDebugTask(lower) {
		return complexStrategy(TaskClassDebugTask, ToolModeFull, PlanPolicyRequired, explicitWeb, channel, "debugging or repair task")
	}
	if looksLikeRepoTask(lower) {
		return complexStrategy(TaskClassRepoTask, ToolModeFull, PlanPolicyOptional, explicitWeb, channel, "local project or repository task")
	}
	if explicitWeb {
		policy := baseStrategy(TaskClassExternalLookup, ToolModeWeb, PlanPolicyOptional, WebPolicyEnabled, channel, "explicit external lookup requested")
		if isSimpleOneShotText(clean) {
			policy.PlanPolicy = PlanPolicyDisabled
			policy.MaxIterations = 2
		}
		policy.AllowedTools = webToolSet(policy.PlanPolicy)
		return policy
	}
	if looksLikeCodingExample(lower) {
		return directAnswerStrategy(TaskClassCodingExample, channel, "small coding example or snippet")
	}
	if looksLikeStableAdvice(lower) {
		return directAnswerStrategy(TaskClassStableAdvice, channel, "stable explanation or advice")
	}
	if looksLikeIdentityQuestion(lower) || isSimpleOneShotText(clean) {
		return directAnswerStrategy(TaskClassSimpleAnswer, channel, "simple one-shot answer")
	}

	return complexStrategy(TaskClassGeneralTask, ToolModeFull, PlanPolicyOptional, false, channel, "general task")
}

func baseStrategy(class TaskClass, mode ToolMode, plan PlanPolicy, web WebPolicy, channel, reason string) TaskStrategy {
	return TaskStrategy{
		Class:                 class,
		ToolMode:              mode,
		PlanPolicy:            plan,
		WebPolicy:             web,
		HiddenTools:           hiddenToolsFor(plan, web),
		RequireProgressEvents: true,
		ChannelMode:           normalizeChannelMode(channel),
		Reason:                reason,
	}
}

func directAnswerStrategy(class TaskClass, channel, reason string) TaskStrategy {
	return TaskStrategy{
		Class:                 class,
		ToolMode:              ToolModeNone,
		PlanPolicy:            PlanPolicyDisabled,
		WebPolicy:             WebPolicyDisabled,
		AllowedTools:          map[string]bool{},
		HiddenTools:           hiddenToolsFor(PlanPolicyDisabled, WebPolicyDisabled),
		MaxIterations:         1,
		RequireProgressEvents: false,
		ChannelMode:           normalizeChannelMode(channel),
		Reason:                reason,
	}
}

func complexStrategy(class TaskClass, mode ToolMode, plan PlanPolicy, explicitWeb bool, channel, reason string) TaskStrategy {
	web := WebPolicyDisabled
	if explicitWeb {
		web = WebPolicyEnabled
	}
	policy := baseStrategy(class, mode, plan, web, channel, reason)
	if mode == ToolModeLocalRead {
		policy.AllowedTools = localReadToolSet(plan)
	}
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

func webToolSet(plan PlanPolicy) map[string]bool {
	allowed := map[string]bool{
		"web_search":  true,
		"web_extract": true,
		"finish_run":  true,
		"tool_search": true,
	}
	if plan != PlanPolicyDisabled {
		allowed["update_plan"] = true
	}
	return allowed
}

func localReadToolSet(plan PlanPolicy) map[string]bool {
	allowed := map[string]bool{
		"read_file":        true,
		"cat":              true,
		"ls_r":             true,
		"list_files":       true,
		"search_files":     true,
		"grep":             true,
		"get_current_time": true,
		"process_list":     true,
		"process_poll":     true,
		"session_search":   true,
		"finish_run":       true,
		"tool_search":      true,
	}
	if plan != PlanPolicyDisabled {
		allowed["update_plan"] = true
	}
	return allowed
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
		s.WebPolicy = WebPolicyEnabled
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
	sb.WriteString("\n# TASK STRATEGY\n")
	sb.WriteString(fmt.Sprintf("Task class: %s. Tool mode: %s. Planning: %s. Web: %s. Channel: %s.\n",
		s.Class, s.ToolMode, s.PlanPolicy, s.WebPolicy, s.ChannelMode))
	if s.ToolMode == ToolModeNone {
		sb.WriteString("Answer directly. Do not call tools for this turn.\n")
	}
	if s.PlanPolicy == PlanPolicyDisabled {
		sb.WriteString("Do not call update_plan for this turn.\n")
	} else if s.PlanPolicy == PlanPolicyRequired {
		sb.WriteString("Use update_plan early, keep it current, and continue through verification or a clear blocker.\n")
	} else {
		sb.WriteString("Use update_plan only when the work genuinely needs multiple steps.\n")
	}
	if s.WebPolicy == WebPolicyDisabled {
		sb.WriteString("Do not use web tools unless the user explicitly asks to search, browse, inspect a URL, or retrieve current external information.\n")
	}
	if s.ChannelMode == "im" {
		sb.WriteString("For IM channels, avoid token-by-token narration; preserve concise progress milestones and final outcomes.\n")
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
		"search", "search web", "browse", "look up", "lookup",
		"latest", "today", "news", "pricing", "price", "release notes",
		"official docs", "official documentation", "current version",
		"\u641c\u7d22", "\u8054\u7f51", "\u4e0a\u7f51", "\u6d4f\u89c8\u7f51\u9875",
		"\u67e5\u4e00\u4e0b", "\u67e5\u8be2", "\u67e5\u627e", "\u627e\u4e00\u4e0b",
		"\u5b98\u7f51", "\u5b98\u65b9\u6587\u6863", "\u6700\u65b0", "\u65b0\u95fb",
		"\u4ef7\u683c", "\u62a5\u4ef7", "\u53d1\u5e03\u8bf4\u660e", "\u5f53\u524d\u7248\u672c",
	})
}

func looksLikeCICDTask(lower string) bool {
	return containsMarker(lower, []string{
		"ci", "cicd", "ci/cd", "pipeline", "github actions", "gitlab ci",
		"deploy", "deployment", "release", "docker", "kubernetes", "k8s",
		"\u6d41\u6c34\u7ebf", "\u90e8\u7f72", "\u53d1\u5e03", "\u4e0a\u7ebf", "\u6784\u5efa",
	})
}

func looksLikeDebugTask(lower string) bool {
	return containsMarker(lower, []string{
		"debug", "fix", "repair", "error", "failed", "failure", "failing",
		"bug", "panic", "stack trace", "regression", "timeout", "unexpected eof",
		"\u8c03\u8bd5", "\u4fee\u590d", "\u62a5\u9519", "\u9519\u8bef", "\u5931\u8d25",
		"\u5f02\u5e38", "\u5361\u4f4f", "\u95ee\u9898",
	})
}

func looksLikeRepoTask(lower string) bool {
	return containsMarker(lower, []string{
		"repo", "repository", "project", "workspace", "local", "file", "directory",
		"readme", "git", "gh ", "branch", "commit", "push", "pull request", "pr",
		"inspect", "check", "look at", "find in", "search in", "refactor", "change",
		"modify", "implement this plan", "continue",
		"\u4ed3\u5e93", "\u9879\u76ee", "\u672c\u5730", "\u6587\u4ef6", "\u76ee\u5f55",
		"\u67e5\u770b", "\u68c0\u67e5", "\u627e\u4e00\u4e0b", "\u641c\u4e00\u4e0b",
		"\u4fee\u6539", "\u91cd\u6784", "\u63d0\u4ea4", "\u63a8\u9001", "\u7ee7\u7eed",
	})
}

func looksLikeCodingExample(lower string) bool {
	return containsMarker(lower, []string{
		"example", "snippet", "sample", "demo", "binary search", "pgsql", "postgres",
		"php", "golang", " go", "rust", "python", "java", "javascript", "typescript",
		"\u793a\u4f8b", "\u4f8b\u5b50", "\u4ee3\u7801", "\u5b9e\u73b0",
		"\u5199\u4e00\u4e2a", "\u5199\u4e00\u6bb5", "\u7528go", "\u7528 go",
		"\u7528php", "\u7528 php", "\u7528rust", "\u7528 rust",
		"\u7528python", "\u7528 python", "\u4e8c\u5206\u6cd5",
		"\u4e8c\u5206\u67e5\u627e", "\u8fde\u63a5pgsql", "\u64cd\u4f5c\u793a\u4f8b",
	})
}

func looksLikeStableAdvice(lower string) bool {
	return containsMarker(lower, []string{
		"explain", "what is", "how to", "guide", "learning plan", "roadmap",
		"advice", "best practice", "skill",
		"\u89e3\u91ca", "\u662f\u4ec0\u4e48", "\u600e\u4e48\u5199",
		"\u600e\u4e48\u505a", "\u5b66\u4e60\u65b9\u6848",
		"\u5b66\u4e60\u8def\u7ebf", "\u5b66\u4e60\u8ba1\u5212",
		"\u65b9\u6848", "\u8def\u7ebf", "\u4f5c\u7528",
	})
}

func looksLikeIdentityQuestion(lower string) bool {
	return containsMarker(lower, []string{
		"who are you", "what model", "which model", "your model",
		"\u4f60\u662f\u8c01", "\u4f60\u662f\u4ec0\u4e48\u6a21\u578b",
		"\u4f60\u7528\u7684\u662f\u4ec0\u4e48\u6a21\u578b",
		"\u4ec0\u4e48\u6a21\u578b",
	})
}

func isSimpleOneShotText(prompt string) bool {
	clean := strings.TrimSpace(prompt)
	if clean == "" {
		return false
	}
	lower := strings.ToLower(clean)
	if looksLikeCICDTask(lower) || looksLikeDebugTask(lower) || looksLikeRepoTask(lower) {
		return false
	}
	if looksLikeCodingExample(lower) || looksLikeStableAdvice(lower) || looksLikeIdentityQuestion(lower) {
		return true
	}
	return len([]rune(clean)) <= 80 && !strings.Contains(clean, "\n")
}

func containsMarker(text string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
