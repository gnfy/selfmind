package router

import (
	"context"
	"fmt"
	"runtime/debug"
	"strings"

	"selfmind/internal/kernel"
	"selfmind/internal/kernel/identity"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/task"
	"selfmind/internal/platform/log"
	"selfmind/internal/runpool"
)

// recoverStreamPanic converts a panic on an agent streaming goroutine into a
// stream error event instead of crashing the entire gateway daemon. These
// goroutines run detached from any net/http per-request recover, so an
// unrecovered panic in runConversation (the agent turn) would take the whole
// process down; the caller's stream aggregation instead sees the Err and
// finalizes the run as failed / task interrupted. Deferred AFTER
// `defer close(respChan)` so it runs FIRST and can still send before close.
func recoverStreamPanic(respChan chan<- llm.StreamEvent) {
	if r := recover(); r != nil {
		log.Error("agent streaming goroutine panicked; recovered to keep the gateway alive",
			"panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
		respChan <- llm.StreamEvent{Err: fmt.Errorf("run aborted by internal error")}
	}
}

// Gateway is the lightweight routing facade used by CLI/HTTP/IM before a
// message enters the durable control-plane flow.
type Gateway struct {
	identityMapper   *identity.IdentityMapper
	taskManager      *task.Manager
	intentClassifier *IntentClassifier
	agent            *kernel.Agent
	llmProvider      llm.Provider
	modelProvider    string
	modelName        string

	// Worker pool (W1b). nil = single-agent serialized path (default, unchanged).
	// When enabled (SELFMIND_WORKERS>1), runs check out a worker agent from
	// `agents`, scheduled by `pool` (per-workspace serialized, bounded
	// concurrency). See docs/worker-pool-design.md.
	pool   *runpool.Pool
	agents chan *kernel.Agent
}

// EnableWorkerPool turns on multi-worker execution with the primary agent plus
// the given extra worker agents. A no-op when extra is empty (keeps the
// default single-agent path), so default behavior is unchanged.
func (g *Gateway) EnableWorkerPool(extra []*kernel.Agent) {
	if g == nil || g.agent == nil || len(extra) == 0 {
		return
	}
	all := append([]*kernel.Agent{g.agent}, extra...)
	g.agents = make(chan *kernel.Agent, len(all))
	for _, a := range all {
		g.agents <- a
	}
	g.pool = runpool.New(len(all))
}

// runConversation runs one turn. With no worker pool it calls the single shared
// agent exactly as before; with a pool it acquires a slot (serialized per
// workspace), checks out a worker agent, runs, and returns it.
func (g *Gateway) runConversation(ctx context.Context, uid, channel, input string) (string, llm.UsageStats, error) {
	if g.pool == nil {
		return g.agent.RunConversation(ctx, uid, channel, input)
	}
	var (
		resp   string
		usage  llm.UsageStats
		runErr error
	)
	poolErr := g.pool.Run(ctx, workspaceSerialKey(ctx), func() error {
		ag := <-g.agents
		defer func() { g.agents <- ag }()
		resp, usage, runErr = ag.RunConversation(ctx, uid, channel, input)
		return runErr
	})
	if poolErr != nil && runErr == nil {
		// Cancelled while queued (or pool returned before running fn).
		return "", usage, poolErr
	}
	return resp, usage, runErr
}

// workspaceSerialKey serializes WRITE turns on the same workspace; an empty key
// means the turn runs in parallel with any other. Read-only turns (no tools,
// web-only, or local-read per the per-turn TaskStrategy) return an empty key so
// concurrent readers of the same workspace are not needlessly serialized —
// matching codex's Exclusive-vs-SharedRead distinction. When no strategy is
// pinned (unknown surface), we conservatively serialize, since an agent turn
// could write.
func workspaceSerialKey(ctx context.Context) string {
	ws, ok := kernel.WorkspaceContextFromContext(ctx)
	if !ok || ws.ID == "" {
		return ""
	}
	if strategy, ok := kernel.TaskStrategyFromContext(ctx); ok && !strategy.MayWriteWorkspace() {
		return "" // read-only turn: safe to run concurrently on this workspace
	}
	return ws.ID
}

func NewGateway(
	identityMapper *identity.IdentityMapper,
	taskManager *task.Manager,
	agent *kernel.Agent,
	llmProvider llm.Provider,
) *Gateway {
	return &Gateway{
		identityMapper:   identityMapper,
		taskManager:      taskManager,
		intentClassifier: NewIntentClassifier(),
		agent:            agent,
		llmProvider:      llmProvider,
	}
}

func (g *Gateway) SetModelDisplay(provider, model string) {
	if g == nil {
		return
	}
	g.modelProvider = strings.TrimSpace(provider)
	g.modelName = strings.TrimSpace(model)
}

func (g *Gateway) SetIntentClassifier(classifier *IntentClassifier) {
	if g == nil || classifier == nil {
		return
	}
	g.intentClassifier = classifier
}

func (g *Gateway) ClassifyIntent(input string) IntentResult {
	if g == nil || g.intentClassifier == nil {
		return NewIntentClassifier().ClassifyDetailed(input)
	}
	return g.intentClassifier.ClassifyDetailed(input)
}

type HandleResponse struct {
	Content      string
	Usage        llm.UsageStats
	Stream       <-chan llm.StreamEvent
	IsStreaming  bool
	Intent       Intent
	IntentReason string
}

// Handle classifies intent and runs the agent/skill/query path. It has NO
// control-command detection: fed "/status" it would create a task, not answer
// the status card. That is safe ONLY because the single live caller — the
// in-process TUI via HandleWithEvents — intercepts every slash/control command
// before reaching here (see internal/gateway/cli handleCommand).
//
// DEAD (2026-07-05): the IM adapters that route raw inbound text straight into
// Handle — internal/gateway/telegram, internal/gateway/wechat, and
// internal/platform/wechat via internal/gateway/channel.Bridge — are UNMOUNTED
// (no non-test importer; GatewayDeps.Bridge is created but never consumed). The
// live inbound funnel is httpapi ProcessMessage → tryHandleControlCommand, which
// owns control detection. Do NOT wire Handle to a raw inbound endpoint without
// first routing through that funnel, or control commands become tasks. Removing
// the dead adapters is a separate cleanup (touches app.GatewayDeps wiring); see
// docs/STATUS.md item 11.
func (g *Gateway) Handle(ctx context.Context, unifiedUID, channel, input string) (*HandleResponse, error) {
	result := g.ClassifyIntent(input)
	intent, reason := result.Intent, result.Reason
	switch intent {
	case IntentSkill:
		content, usage, err := g.handleSkill(ctx, unifiedUID, channel, input)
		return &HandleResponse{Content: content, Usage: usage, Intent: intent, IntentReason: reason}, err
	case IntentQuery:
		content, usage, err := g.handleQuery(ctx, unifiedUID, channel, input)
		return &HandleResponse{Content: content, Usage: usage, Intent: intent, IntentReason: reason}, err
	case IntentRoute:
		content, usage, err := g.handleRoute(ctx, unifiedUID, channel, input)
		return &HandleResponse{Content: content, Usage: usage, Intent: intent, IntentReason: reason}, err
	case IntentContinue, IntentTask, IntentCasual:
		return g.handleTaskStreaming(ctx, unifiedUID, channel, input, intent, reason)
	default:
		return g.handleTaskStreaming(ctx, unifiedUID, channel, input, IntentTask, "agent-first fallback")
	}
}

func (g *Gateway) handleTaskStreaming(ctx context.Context, unifiedUID, channel, input string, intent Intent, reason string) (*HandleResponse, error) {
	if g == nil || g.agent == nil {
		return nil, fmt.Errorf("gateway agent is not configured")
	}
	if g.taskManager == nil {
		return g.runAgentStreaming(ctx, unifiedUID, channel, input, intent, reason)
	}

	var taskID int64
	var err error
	if intent == IntentContinue {
		t, _, err := g.taskManager.GetCurrentTask(ctx, unifiedUID)
		if err != nil || t == nil {
			return &HandleResponse{Content: "No active task to continue. Tell me what you want to work on next."}, nil
		}
		taskID = t.ID
	} else {
		taskID, err = g.taskManager.CreateTask(ctx, unifiedUID, extractTitle(input))
		if err != nil {
			return nil, err
		}
	}
	g.taskManager.AppendContext(ctx, unifiedUID, channel, "user", input)

	respChan := make(chan llm.StreamEvent, 256)
	go func() {
		defer close(respChan)
		defer recoverStreamPanic(respChan)
		resp, usage, err := g.runConversation(ctx, unifiedUID, channel, input)
		if err != nil {
			respChan <- llm.StreamEvent{Err: err}
			if intent == IntentTask {
				g.taskManager.UpdateTaskStatus(ctx, unifiedUID, taskID, "failed")
			}
			return
		}
		g.taskManager.AppendContext(ctx, unifiedUID, channel, "assistant", resp)
		if isTaskDone(resp) {
			g.taskManager.UpdateTaskStatus(ctx, unifiedUID, taskID, "done")
		}
		if resp != "" {
			respChan <- llm.StreamEvent{Content: resp}
		}
		respChan <- llm.StreamEvent{Usage: &usage}
	}()

	return &HandleResponse{
		IsStreaming:  true,
		Stream:       respChan,
		Intent:       intent,
		IntentReason: reason,
	}, nil
}

func (g *Gateway) handleCasual(ctx context.Context, unifiedUID, channel, input string) (string, llm.UsageStats, error) {
	reply := g.casualReply(input)
	if g.taskManager != nil {
		_ = g.taskManager.SaveCasualSummary(ctx, unifiedUID, channel, "casual: "+input)
	}
	return reply, llm.UsageStats{}, nil
}

func (g *Gateway) RunAgent(ctx context.Context, unifiedUID, channel, input string) (*HandleResponse, error) {
	return g.runAgentStreaming(ctx, unifiedUID, channel, input, IntentTask, "agent-only")
}

func (g *Gateway) runAgentStreaming(ctx context.Context, unifiedUID, channel, input string, intent Intent, reason string) (*HandleResponse, error) {
	if g == nil || g.agent == nil {
		return nil, fmt.Errorf("gateway agent is not configured")
	}
	respChan := make(chan llm.StreamEvent, 256)
	go func() {
		defer close(respChan)
		defer recoverStreamPanic(respChan)
		resp, usage, err := g.runConversation(ctx, unifiedUID, channel, input)
		if err != nil {
			respChan <- llm.StreamEvent{Err: err}
			return
		}
		if resp != "" {
			respChan <- llm.StreamEvent{Content: resp}
		}
		respChan <- llm.StreamEvent{Usage: &usage}
	}()
	return &HandleResponse{
		IsStreaming:  true,
		Stream:       respChan,
		Intent:       intent,
		IntentReason: reason,
	}, nil
}

func isModelStatusQuestion(input string) bool {
	cleaned := normalizeQuestionText(input)
	if cleaned == "" {
		return false
	}
	if !containsAnyNormalized(cleaned, []string{"model", "llm", "\u6a21\u578b", "\u5927\u6a21\u578b", "\u540e\u7aef"}) {
		return false
	}
	return containsAnyNormalized(cleaned, []string{
		"what", "which", "current", "using", "running", "active",
		"\u4ec0\u4e48", "\u54ea\u4e2a", "\u54ea\u4e00\u4e2a", "\u5f53\u524d", "\u73b0\u5728", "\u76ee\u524d",
		"\u6b63\u5728\u7528", "\u7528\u7684", "\u4f7f\u7528\u7684", "\u8fde\u63a5\u7684", "\u8dd1\u7684",
	})
}

func normalizeQuestionText(input string) string {
	input = strings.ToLower(strings.TrimSpace(input))
	replacer := strings.NewReplacer(
		"\u3000", "",
		" ", "",
		"\t", "",
		"\n", "",
		"\r", "",
		"?", "",
		"\uff1f", "",
		"!", "",
		"\uff01", "",
		"\u3002", "",
		".", "",
		"\uff0e", "",
		",", "",
		"\uff0c", "",
		":", "",
		"\uff1a", "",
		";", "",
	)
	return replacer.Replace(input)
}

func containsAnyNormalized(value string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func (g *Gateway) modelStatusReply() string {
	return modelStatusReplyText(g.modelDisplayLabel())
}

func (g *Gateway) ModelStatusReply() string {
	if g == nil {
		return modelStatusReplyText("")
	}
	return g.modelStatusReply()
}

func modelStatusReplyText(label string) string {
	if strings.TrimSpace(label) == "" {
		return "I am SelfMind. No usable AI model configuration was resolved. Run `selfmind model check` to inspect the problem."
	}
	return fmt.Sprintf("I am SelfMind. The current model is %s.", label)
}

func (g *Gateway) modelDisplayLabel() string {
	provider := strings.TrimSpace(g.modelProvider)
	model := strings.TrimSpace(g.modelName)
	switch {
	case provider != "" && model != "" && provider != "not configured":
		return provider + "/" + model
	case model != "" && model != "not configured":
		return model
	case provider != "" && provider != "not configured":
		return provider
	default:
		return ""
	}
}

func (g *Gateway) casualReply(input string) string {
	if reply, ok := g.directCasualReply(input); ok {
		return reply
	}
	return "I understand. Tell me what you want me to inspect, change, test, or continue."
}

func (g *Gateway) directCasualReply(input string) (string, bool) {
	switch normalizeQuestionText(input) {
	case "\u4f60\u597d", "\u60a8\u597d", "hi", "hello", "\u55e8", "hey":
		return "Hello, I am SelfMind. I can help with development tasks, code inspection, tool execution, and multi-device collaboration.", true
	case "\u4f60\u662f\u8c01", "\u4f60\u53eb\u4ec0\u4e48", "\u4f60\u662f\u5e72\u561b\u7684", "whoareyou", "whatareyou":
		reply := "I am SelfMind, an AI work assistant for development tasks and multi-device collaboration."
		if label := g.modelDisplayLabel(); label != "" {
			reply += " The current model is " + label + "."
		}
		return reply, true
	case "\u8c22\u8c22", "\u591a\u8c22", "\u8c22\u4e86", "thanks", "thankyou":
		return "You're welcome. Tell me directly when you want to continue a task.", true
	case "\u518d\u89c1", "\u62dc\u62dc", "bye", "\u665a\u5b89":
		return "Goodbye. Come back whenever you need help.", true
	}
	return "", false
}

func (g *Gateway) handleSkill(ctx context.Context, unifiedUID, channel, input string) (string, llm.UsageStats, error) {
	skillName := trimKnownPrefix(input, []string{"/skill ", "/s "})
	if skillName == "" {
		return "Please specify the skill to run.", llm.UsageStats{}, nil
	}
	if g == nil || g.agent == nil || g.agent.Dispatcher() == nil {
		return "", llm.UsageStats{}, fmt.Errorf("skill dispatcher is not configured")
	}
	resp, err := g.agent.Dispatcher().Dispatch("skill:"+skillName, map[string]interface{}{
		"input":      skillName,
		"_tenant_id": unifiedUID,
	})
	return resp, llm.UsageStats{}, err
}

// DispatchTool runs a single management tool through the agent's backend and
// returns its text result. It powers daemon-side execution of agent-backed
// slash commands (/skills, /memory subcommands, /bundles, /curator,
// /checkpoint) so a thin TUI client can run them without an in-process agent.
// It is NOT a full agent turn — no per-run mutable state is touched — so it is
// safe to call concurrently alongside the worker pool. Callers (the HTTP
// handler) are responsible for restricting which tools may be dispatched and for
// setting the tenant scope in args.
func (g *Gateway) DispatchTool(tool string, args map[string]interface{}) (string, error) {
	if g == nil || g.agent == nil || g.agent.Dispatcher() == nil {
		return "", fmt.Errorf("gateway agent is not configured")
	}
	return g.agent.Dispatcher().Dispatch(tool, args)
}

func (g *Gateway) handleQuery(ctx context.Context, unifiedUID, channel, input string) (string, llm.UsageStats, error) {
	query := trimKnownPrefix(input, []string{"/query ", "/search "})
	if query == "" {
		query = strings.TrimSpace(input)
	}
	if g == nil || g.agent == nil || g.agent.Dispatcher() == nil {
		return "", llm.UsageStats{}, fmt.Errorf("session search is not configured")
	}
	resp, err := g.agent.Dispatcher().Dispatch("session_search", map[string]interface{}{
		"query":      query,
		"limit":      10,
		"_tenant_id": unifiedUID,
	})
	return resp, llm.UsageStats{}, err
}

func (g *Gateway) handleRoute(ctx context.Context, unifiedUID, channel, input string) (string, llm.UsageStats, error) {
	return fmt.Sprintf("Route command received. Current channel: %s.", channel), llm.UsageStats{}, nil
}

func trimKnownPrefix(input string, prefixes []string) string {
	value := strings.TrimSpace(input)
	lower := strings.ToLower(value)
	for _, prefix := range prefixes {
		prefix = strings.TrimSpace(prefix)
		if prefix == "" {
			continue
		}
		if strings.HasPrefix(lower, strings.ToLower(prefix)) {
			return strings.TrimSpace(value[len(prefix):])
		}
	}
	return value
}

func (g *Gateway) ResolveUID(ctx context.Context, platform, platformID string) (string, error) {
	return g.identityMapper.EnsureBound(ctx, platform, platformID)
}

func (g *Gateway) ListTasks(ctx context.Context, unifiedUID string) ([]task.Task, error) {
	return g.taskManager.ListTasks(ctx, unifiedUID)
}

func (g *Gateway) GetCurrentTaskInfo(ctx context.Context, unifiedUID string) (*task.Task, error) {
	tt, _, err := g.taskManager.GetCurrentTask(ctx, unifiedUID)
	return tt, err
}

func isTaskDone(response string) bool {
	return taskResponseLooksComplete(response)
}

func taskResponseLooksComplete(response string) bool {
	trimmed := strings.TrimSpace(response)
	lower := strings.ToLower(trimmed)
	if lower == "" {
		return false
	}
	if taskResponseContainsAny(lower, []string{
		"not done", "not completed", "not finished", "remaining work", "still need",
		"need to continue", "next steps", "todo:", "blocked",
	}) || taskResponseContainsAny(trimmed, []string{
		"\u672a\u5b8c\u6210", "\u8fd8\u6ca1\u5b8c\u6210", "\u6ca1\u6709\u5b8c\u6210",
		"\u5f85\u5b8c\u6210", "\u9700\u8981\u7ee7\u7eed", "\u963b\u585e",
	}) {
		return false
	}
	if lower == "done" || lower == "completed" || lower == "finished" || lower == "all done" {
		return true
	}
	return taskResponseContainsAny(lower, []string{
		"task complete", "task completed", "completed successfully",
		"finished successfully", "all done", "implementation complete", "tests pass",
	}) || taskResponseContainsAny(trimmed, []string{
		"\u5df2\u5b8c\u6210", "\u4efb\u52a1\u5b8c\u6210", "\u5904\u7406\u5b8c\u6210",
		"\u5df2\u5904\u7406\u5b8c", "\u5df2\u7ecf\u5b8c\u6210", "\u641e\u5b9a",
	})
}

func taskResponseContainsAny(value string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func extractTitle(input string) string {
	title := strings.TrimSpace(input)
	for _, prefix := range []string{"please"} {
		if strings.HasPrefix(strings.ToLower(title), strings.ToLower(prefix)) {
			title = strings.TrimSpace(title[len(prefix):])
			break
		}
	}
	if len([]rune(title)) > 30 {
		runes := []rune(title)
		title = string(runes[:30]) + "..."
	}
	if title == "" {
		return "New task"
	}
	return title
}

func (g *Gateway) QuickReply(ctx context.Context, unifiedUID, channel, input string) (*HandleResponse, error) {
	return g.Handle(ctx, unifiedUID, channel, input)
}
