package kernel

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/textutil"
)

// AgentBackend is the interface for the agent's execution backend (tool dispatch + event channel).
// Concrete implementation is provided by the tools package via NewAgentBackend.
type AgentBackend interface {
	Dispatch(name string, args map[string]interface{}) (string, error)
	GetToolDefinitions() []map[string]interface{}
}

// Agent 核心推理循环
type Agent struct {
	memory             *memory.MemoryManager
	backend            AgentBackend
	llm                llm.Provider
	fastProvider       llm.Provider                 // optional fast model for simple direct-answer turns
	summaryProvider    llm.Provider                 // optional cheap model for over-budget context compaction, kept OFF the main run provider
	judgeProvider      llm.Provider                 // optional cheap model for smart-mode approval triage (H2), kept OFF the main run provider
	runLLM             llm.Provider                 // per-run active provider, set under runMu
	skillInventory     func(tenantID string) string // optional: compact learned-skill list for the prompt
	profileSynthesizer *ProfileSynthesizer          // optional: distills facts into a user profile
	soul               string
	maxIterations      int
	maxRetries         int
	retryBase          time.Duration // LLM retry backoff base (0 => llm.DefaultRetryBase)
	retryCap           time.Duration // LLM retry backoff cap (0 => llm.DefaultRetryCap)
	Reflector          *ReflectionEngine
	ReviewEngine       *BackgroundReviewEngine
	contextEngine      *ContextEngine
	contextScanner     *ContextScanner
	factExtractor      *FactExtractor
	turnExtractor      *TurnExtractor
	semanticExpander   *memory.SemanticExpander
	useMemoryFence     bool
	EventChannel       chan string // emits "tool_start:name" and "tool_end:name:result" events
	runMu              sync.Mutex
	syncQueue          chan syncTurnRequest

	// Evolution config.
	toolCallCount     int
	nudgeInterval     int
	turnReviewCount   int
	evolutionNotifyCh chan string
}

type syncTurnRequest struct {
	tenantID string
	data     []byte
}

func NewAgent(mem *memory.MemoryManager, backend AgentBackend, provider llm.Provider, soul string, maxIter, maxRetries int, refl *ReflectionEngine) *Agent {
	ch := make(chan string, agentEventBufferSize)
	ag := &Agent{
		memory:         mem,
		backend:        backend,
		llm:            provider,
		soul:           soul,
		maxIterations:  maxIter,
		maxRetries:     maxRetries,
		Reflector:      refl,
		contextEngine:  NewContextEngine(128000, 512),
		contextScanner: NewContextScanner(),
		EventChannel:   ch,
		syncQueue:      make(chan syncTurnRequest, 16),
		// factExtractor is set via SetFactExtractor after agent creation
		// so the caller can decide whether to enable auto-extraction.
		toolCallCount: 0,
		nudgeInterval: 10, // 默认每 10 次工具调用触发一次
	}
	ag.contextEngine.SetProvider(provider)
	go ag.runSyncWorker()
	return ag
}

// SetNudgeInterval sets how often evolution review triggers (every N tool calls)
func (a *Agent) SetNudgeInterval(n int) {
	if n > 0 {
		a.nudgeInterval = n
	}
}

func (a *Agent) SetBackgroundReviewEngine(engine *BackgroundReviewEngine) {
	a.ReviewEngine = engine
	if engine != nil {
		engine.SetBackend(a.backend)
		engine.SetNotifyChannel(a.evolutionNotifyCh)
		engine.SetUseMemoryFence(a.useMemoryFence)
	}
}

// SetEvolutionNotifyChannel sets the channel for evolution notifications to TUI
// SetFactExtractor injects the auto-fact-extractor. Called by app layer after agent creation.
func (a *Agent) SetFactExtractor(fe *FactExtractor) {
	a.factExtractor = fe
}

// SetTurnExtractor injects the per-turn lightweight fact extractor.
func (a *Agent) SetTurnExtractor(te *TurnExtractor) {
	a.turnExtractor = te
}

// SetSemanticExpander injects the query semantic expander for recall.
func (a *Agent) SetSemanticExpander(se *memory.SemanticExpander) {
	a.semanticExpander = se
}

// SetUseMemoryFence enables/disables the <memory-context> fence format in system prompt.
func (a *Agent) SetUseMemoryFence(enabled bool) {
	a.useMemoryFence = enabled
	if a.ReviewEngine != nil {
		a.ReviewEngine.SetUseMemoryFence(enabled)
	}
}

// SetFastProvider installs an optional fast model used for simple, pure
// direct-answer turns. It is a role-resolved provider that falls back to the
// default model when no fast model is configured, so callers always get a
// working provider.
func (a *Agent) SetFastProvider(p llm.Provider) {
	if a == nil {
		return
	}
	a.fastProvider = p
}

// SetSummaryProvider installs the cheap provider used to compact over-budget
// context into a summary (the memory_extract role, kept OFF the main coding
// provider). It is remembered so SetContextWindow, which rebuilds the context
// engine, can re-apply it. When unset the context engine falls back to
// deterministic trimming.
func (a *Agent) SetSummaryProvider(p llm.Provider) {
	if a == nil {
		return
	}
	a.summaryProvider = p
	if a.contextEngine != nil {
		a.contextEngine.SetSummaryProvider(p)
	}
}

// SetApprovalJudgeProvider installs the cheap role-routed provider used for
// smart-mode approval triage (H2). The kernel only carries the provider so the
// app/gateway layer (which owns model routing) can pick a cheap role and keep it
// OFF the run's main coding provider; the concrete tools.ApprovalJudge is built
// at the gateway boundary from ApprovalJudgeProvider. Kept as an llm.Provider,
// not a tools type, so kernel takes no dependency on concrete tools.
func (a *Agent) SetApprovalJudgeProvider(p llm.Provider) {
	if a == nil {
		return
	}
	a.judgeProvider = p
}

// ApprovalJudgeProvider returns the cheap provider for smart-mode approval
// triage, or nil when none is configured (then smart mode degrades to a human
// ask — never an auto-approval).
func (a *Agent) ApprovalJudgeProvider() llm.Provider {
	if a == nil {
		return nil
	}
	return a.judgeProvider
}

// SummaryProvider returns the cheap memory_extract-role provider installed by
// SetSummaryProvider, or nil when none is configured. The gateway boundary
// uses it to build side-task judges that must stay OFF the main coding
// provider (e.g. the post-run labeler, Work Timeline P3); a nil provider makes
// those features degrade to no-ops, never to the main model.
func (a *Agent) SummaryProvider() llm.Provider {
	if a == nil {
		return nil
	}
	return a.summaryProvider
}

// activeLLM returns the provider for the current run (the per-run choice when
// set, else the default coding provider).
func (a *Agent) activeLLM() llm.Provider {
	if a != nil && a.runLLM != nil {
		return a.runLLM
	}
	return a.llm
}

// chooseRunProvider routes pure simple-answer turns to the fast provider when
// one is available; everything else uses the default coding provider.
func (a *Agent) chooseRunProvider(strategy TaskStrategy) llm.Provider {
	if a.fastProvider != nil && strategy.Class == TaskClassSimpleAnswer {
		return a.fastProvider
	}
	return a.llm
}

// SetSkillInventory installs a callback that returns a compact list of the
// tenant's learned skills. When set, the list is injected into the system
// prompt on tool-bearing turns so the agent knows what it has already learned
// and can load a skill with skill_view instead of relearning it. kernel stays
// independent of the tools package; the app provides the closure.
func (a *Agent) SetSkillInventory(fn func(tenantID string) string) {
	if a == nil {
		return
	}
	a.skillInventory = fn
}

// SetProfileSynthesizer installs the (optional) user-profile synthesizer.
func (a *Agent) SetProfileSynthesizer(p *ProfileSynthesizer) {
	if a == nil {
		return
	}
	a.profileSynthesizer = p
}

// SetContextWindow aligns compaction with the resolved model context length.
// The provider resolver owns model-specific values; the agent uses this only
// for local budgeting and summary/truncation decisions.
//
// The compaction budget is capped at a WORKING budget (default 256K, override
// via SELFMIND_CONTEXT_BUDGET) rather than the full model window. On a very
// large window (e.g. codex ~1.05M) letting the transcript grow to ~900K before
// trimming is wasteful — especially for stateless providers (store=false) that
// re-send the whole transcript every step. Capping keeps recent context + the
// system prompt while bounding per-call token cost. Models with a smaller
// window still use their own (smaller) window.
func (a *Agent) SetContextWindow(maxTokens int) {
	if a == nil || maxTokens <= 0 {
		return
	}
	budget := agentContextBudget(maxTokens)
	a.contextEngine = NewContextEngine(budget, contextReserveTokens(budget))
	a.contextEngine.SetProvider(a.llm)
	// Re-apply the cheap compaction summarizer onto the fresh engine so default
	// over-budget compaction survives a context-window reconfiguration.
	a.contextEngine.SetSummaryProvider(a.summaryProvider)
}

// agentContextBudget returns the working compaction budget: min(model window,
// configured working budget). Default working budget is 256K tokens.
func agentContextBudget(modelWindow int) int {
	working := 256000
	if v := strings.TrimSpace(os.Getenv("SELFMIND_CONTEXT_BUDGET")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			working = n
		}
	}
	if modelWindow > 0 && modelWindow < working {
		return modelWindow
	}
	return working
}

func contextReserveTokens(maxTokens int) int {
	reserve := maxTokens / 8
	if reserve < 1024 {
		reserve = 1024
	}
	if reserve > 32768 {
		reserve = 32768
	}
	return reserve
}

// SwitchModel changes the underlying LLM model at runtime if the provider supports it.
func (a *Agent) SwitchModel(modelName string) bool {
	return llm.SetModelName(a.llm, modelName)
}

// CurrentModel returns the active model name (if exposed by the provider).
func (a *Agent) CurrentModel() string {
	return llm.GetModelName(a.llm)
}

// Provider exposes the agent's model transport for lightweight gateway routing.
func (a *Agent) Provider() llm.Provider {
	if a == nil {
		return nil
	}
	return a.llm
}

func (a *Agent) SetEvolutionNotifyChannel(ch chan string) {
	a.evolutionNotifyCh = ch
	if a.Reflector != nil {
		a.Reflector.SetNotifyChannel(ch)
	}
	if a.ReviewEngine != nil {
		a.ReviewEngine.SetNotifyChannel(ch)
	}
}

const MaxRetries = 3
const agentEventBufferSize = 1024

// SetRetryPolicy configures the LLM transport retry loop: attempt count and the
// exponential-backoff base/cap. Non-positive values keep the current/default
// value (attempts default to a.maxRetries; base/cap default to
// llm.DefaultRetryBase / llm.DefaultRetryCap). Injected once by internal/app
// from config; kernel owns no config parsing.
func (a *Agent) SetRetryPolicy(attempts int, base, cap time.Duration) {
	if attempts > 0 {
		a.maxRetries = attempts
	}
	if base > 0 {
		a.retryBase = base
	}
	if cap > 0 {
		a.retryCap = cap
	}
}

// retryAttempts returns the effective attempt count (>=1).
func (a *Agent) retryAttempts() int {
	if a.maxRetries > 0 {
		return a.maxRetries
	}
	return 1
}

// waitBeforeRetry sleeps before retry number attempt (1-based). It honors a
// server-advertised Retry-After (from the just-failed err) when present,
// otherwise exponential backoff base*2^(attempt-1) with [0.9,1.1) jitter,
// capped. The sleep is context-cancellable so /stop and deadlines interrupt a
// pending backoff. Returns ctx.Err() if the context ended mid-wait.
func (a *Agent) waitBeforeRetry(ctx context.Context, attempt int, err error) error {
	delay := llm.Backoff(attempt, a.retryBase, a.retryCap, rand.Float64)
	if ra, ok := llm.RetryAfterFromError(err); ok && ra > delay {
		delay = ra
	}
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// chatResponseWithRetry implements retry for non-streaming model calls. It only
// re-sends on retryable (transient) errors with exponential backoff + jitter;
// fatal errors (context-window, quota, auth, invalid request) fail fast so we
// never waste attempts on unfixable calls.
func (a *Agent) chatResponseWithRetry(ctx context.Context, messages []llm.Message, strategy TaskStrategy) (*llm.ChatResponse, error) {
	var lastErr error
	max := a.retryAttempts()
	messages = a.prepareMessagesForModel(messages)
	for attempt := 1; attempt <= max; attempt++ {
		req := llm.ChatRequest{Messages: messages, Tools: a.llmToolDefinitions(strategy)}
		resp, err := a.activeLLM().Chat(ctx, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err

		// Context cancel/deadline: stop immediately, do not retry.
		if ctx.Err() != nil {
			return nil, err
		}
		// Fatal (non-retryable) provider errors fail fast.
		if !llm.IsRetryableError(err) {
			return nil, err
		}
		if attempt == max {
			break
		}
		if werr := a.waitBeforeRetry(ctx, attempt, err); werr != nil {
			return nil, werr
		}
	}
	return nil, fmt.Errorf("llm chat failed after %d attempts: %w", max, lastErr)
}

// chatWithRetry 实现了运行时自动 Fallback 逻辑
func (a *Agent) chatWithRetry(ctx context.Context, messages []llm.Message) (string, llm.UsageStats, error) {
	resp, err := a.chatResponseWithRetry(ctx, messages, DefaultTaskStrategy())
	if err != nil {
		return "", llm.UsageStats{}, err
	}
	return resp.Content, resp.Usage, nil
}

// streamChatWithRetry opens a streaming call, re-sending on retryable errors
// with exponential backoff + jitter. This is where the codex
// `Post .../responses: EOF` connection drops are absorbed: a dropped POST is a
// retryable error, so the loop reconnects with a full re-send (store=false
// means there is no server-persisted response to resume by cursor). Fatal
// errors fail fast.
func (a *Agent) streamChatWithRetry(ctx context.Context, messages []llm.Message, strategy TaskStrategy) (<-chan llm.StreamEvent, error) {
	var lastErr error
	max := a.retryAttempts()
	messages = a.prepareMessagesForModel(messages)
	for attempt := 1; attempt <= max; attempt++ {
		req := llm.ChatRequest{Messages: messages, Tools: a.llmToolDefinitions(strategy)}
		ch, err := a.activeLLM().StreamChat(ctx, req)
		if err == nil {
			return ch, nil
		}
		lastErr = err

		if ctx.Err() != nil {
			return nil, err
		}
		if !llm.IsRetryableError(err) {
			return nil, err
		}
		if attempt == max {
			break
		}
		if werr := a.waitBeforeRetry(ctx, attempt, err); werr != nil {
			return nil, werr
		}
	}
	return nil, fmt.Errorf("llm stream chat failed after %d attempts: %w", max, lastErr)
}

func (a *Agent) prepareMessagesForModel(messages []llm.Message) []llm.Message {
	if a == nil || a.contextEngine == nil {
		return messages
	}
	return a.contextEngine.TruncateMessages(messages)
}

// SetBackend updates the agent's execution backend
func (a *Agent) SetBackend(b AgentBackend) {
	a.backend = b
	if a.ReviewEngine != nil {
		a.ReviewEngine.SetBackend(b)
	}
}

// Dispatcher returns the tool dispatch backend for gateway handlers and skill tools.
func (a *Agent) Dispatcher() AgentBackend {
	return a.backend
}

// Memory returns the agent's memory manager.
func (a *Agent) Memory() *memory.MemoryManager {
	return a.memory
}

// Analyze implements tools.VisionLLM
func (a *Agent) Analyze(imageBase64, mimeType, question string) (string, error) {
	msg := llm.Message{
		Role: "user",
		MultiContent: []llm.ContentPart{
			{
				Type:     "image_base64",
				MimeType: mimeType,
				Data:     imageBase64,
			},
			{
				Type: "text",
				Text: question,
			},
		},
	}

	// Call LLM with the multimodal message
	return a.llm.ChatCompletion(context.Background(), []llm.Message{msg})
}

func emitToolEndEventWithDuration(ch chan string, name, toolCallID string, result ToolResultEnvelope, duration float64, err error) {
	if err != nil {
		category, hint := classifyToolFailure(err.Error())
		EmitAgentEvent(ch, AgentEvent{
			Type:            "tool.completed",
			ToolName:        name,
			ToolCallID:      toolCallID,
			DurationSeconds: duration,
			ToolResult:      result.Preview,
			Error:           result.ModelContent,
			Payload: map[string]interface{}{
				"result_bytes":     result.Bytes,
				"result_truncated": result.Truncated,
				"error_category":   category,
				"diagnostic_hint":  hint,
			},
		})
	} else {
		EmitAgentEvent(ch, AgentEvent{
			Type:            "tool.completed",
			ToolName:        name,
			ToolCallID:      toolCallID,
			ToolResult:      result.Preview,
			DurationSeconds: duration,
			Payload: map[string]interface{}{
				"result_bytes":     result.Bytes,
				"result_truncated": result.Truncated,
			},
		})
	}
}

func classifyToolFailure(message string) (string, string) {
	lower := strings.ToLower(strings.TrimSpace(message))
	switch {
	case lower == "":
		return "unknown", "Inspect the tool input and retry with corrected arguments if useful."
	case strings.Contains(lower, "escapes workspace") || strings.Contains(lower, "permission") || strings.Contains(lower, "denied"):
		return "workspace_scope", "Inspect the active workspace root and use a path inside the allowed workspace."
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline exceeded"):
		return "timeout", "Narrow the command or inspect progress before retrying with a smaller scope."
	case strings.Contains(lower, "exit status") || strings.Contains(lower, "command failed"):
		return "command_failed", "Treat the output as evidence; inspect cwd, env, files, or command help before retrying."
	case strings.Contains(lower, "missing required parameter") || strings.Contains(lower, "invalid schema") || strings.Contains(lower, "schema"):
		return "tool_schema", "Correct the tool arguments using the declared schema before retrying."
	case strings.Contains(lower, "unauthorized") || strings.Contains(lower, "authentication") || strings.Contains(lower, "token_expired") || strings.Contains(lower, "401"):
		return "auth", "Check the relevant credential or login state before retrying."
	default:
		return "unknown", "Inspect the failed tool result and continue with a corrected next step unless this is a blocker."
	}
}

func emitToolEndEvent(ch chan string, name, result string, err error) {
	if err != nil {
		emitToolEndEventWithDuration(ch, name, "", packageToolError(name, err), 0, err)
		return
	}
	emitToolEndEventWithDuration(ch, name, "", packageToolResult(name, result), 0, nil)
}

// RunConversation 执行 Agent 推理循环
// channel 用于渠道隔离的历史记录（如 'cli'、'wechat'、'dingtalk'）
func (a *Agent) RunConversation(ctx context.Context, tenantID, channel string, initialPrompt string) (finalOutput string, finalUsage llm.UsageStats, finalErr error) {
	a.runMu.Lock()
	defer a.runMu.Unlock()

	// Flight recorder: tag this turn so the recorder captures its model calls,
	// and write the turn's metadata when it finishes (no-op unless enabled).
	if finalize := a.beginFlightRecording(&ctx, tenantID, channel, initialPrompt); finalize != nil {
		defer func() { finalize(finalOutput, finalErr) }()
	}

	var totalUsage llm.UsageStats
	eventCh := eventChannelFromContext(ctx, a.EventChannel)
	EmitAgentEvent(eventCh, AgentEvent{
		Type: "turn.started",
		Payload: map[string]interface{}{
			"tenant_id": tenantID,
			"channel":   channel,
		},
	})
	modelCtx := llm.ModelContext{
		TenantID: tenantID,
		Role:     llm.RoleCodingAgent,
	}
	if workspace, ok := WorkspaceContextFromContext(ctx); ok {
		modelCtx.WorkspaceID = workspace.ID
	}
	if runtime, ok := TaskRuntimeContextFromContext(ctx); ok {
		modelCtx.TaskID = runtime.TaskID
		modelCtx.RunID = runtime.RunID
		if modelCtx.WorkspaceID == "" {
			modelCtx.WorkspaceID = runtime.WorkspaceID
		}
	}
	ctx = llm.WithModelContext(ctx, modelCtx)

	strategy, ok := taskStrategyFromContext(ctx)
	if !ok {
		strategy = BuildTaskStrategy(initialPrompt, channel)
	}
	strategy = strategy.normalized()

	// Route this run's model calls per strategy (simple direct-answer turns may
	// use a fast model). Safe because runMu serializes RunConversation.
	a.runLLM = a.chooseRunProvider(strategy)
	defer func() { a.runLLM = nil }()
	EmitAgentEvent(eventCh, AgentEvent{
		Type:    "strategy.selected",
		Content: strategy.Reason,
		Payload: map[string]interface{}{
			"class":       string(strategy.Class),
			"tool_mode":   string(strategy.ToolMode),
			"plan_policy": string(strategy.PlanPolicy),
			"web_policy":  string(strategy.WebPolicy),
			"channel":     strategy.ChannelMode,
			"tool_budget": strategy.normalized().MaxActionTools,
		},
	})

	ctx = a.selectRuntimeContext(ctx, tenantID, channel, initialPrompt, strategy, eventCh)

	// 0. Build dynamic system prompt (including facts + project context)
	systemPrompt, _ := a.buildSystemPrompt(ctx, tenantID, strategy, initialPrompt)
	systemPrompt += strategy.SystemPromptNote()

	// 0.1 Inject project context files (.selfmind.md, AGENTS.md, etc.)
	if a.contextScanner != nil {
		var ctxFiles []ContextFile
		if workspace, ok := WorkspaceContextFromContext(ctx); ok {
			ctxFiles, _ = a.contextScanner.ScanFrom(workspace.Root)
		} else {
			ctxFiles, _ = a.contextScanner.Scan()
		}
		if len(ctxFiles) > 0 {
			ctxPrompt := a.contextScanner.BuildContextPrompt(ctxFiles)
			if ctxPrompt != "" {
				systemPrompt += "\n\n" + ctxPrompt
			}
		}
	}

	// Compose the turn window (ContextComposer contract, context_composer.go):
	// system prompt + person-level spine tail + compaction summary when over
	// budget + the latest user message. Working-context history lives on the
	// person's WORK SPINE, so a task started on WeChat continues from CLI with
	// the same recent context; the fallback chain is the read-only legacy
	// compat path for history that predates the spine.
	histKey := a.trajectoryKey(ctx, channel)
	fallbackKeys := a.trajectoryFallbackKeys(ctx, channel)
	messages, err := a.Composer().Compose(
		ctx, a.memory, tenantID,
		histKey,
		fallbackKeys,
		systemPrompt,
		initialPrompt,
	)
	if err != nil {
		return "", totalUsage, fmt.Errorf("build messages: %w", err)
	}

	history := TaskHistory{
		Goal:  initialPrompt,
		Steps: []string{},
	}
	var continuedAnswer strings.Builder

	maxIterations := a.maxIterations
	if strategy.MaxIterations > 0 && (maxIterations <= 0 || strategy.MaxIterations < maxIterations) {
		maxIterations = strategy.MaxIterations
	}
	if maxIterations < 2 {
		maxIterations = 2
	}
	actionToolsUsed := 0
	toolUseCounts := map[string]int{}
	toolBudgetRepairIssued := false
	steerCh := steeringFromContext(ctx)
	for i := 0; i < maxIterations; i++ {
		// Mid-turn steering: fold any follow-up the user typed while this turn was
		// running into the conversation before the next model call, so the agent
		// adjusts course in-flight instead of the input being rejected or lost.
		for _, guidance := range drainSteering(steerCh) {
			messages = append(messages, llm.Message{Role: "user", Content: guidance})
			history.Steps = append(history.Steps, "user added guidance mid-turn")
			EmitAgentEvent(eventCh, AgentEvent{
				Type:    "agent.steering",
				Payload: map[string]interface{}{"input": guidance},
			})
		}
		iterationStrategy := strategy
		if actionToolBudgetReached(strategy, actionToolsUsed) {
			iterationStrategy = strategy.WithActionToolsDisabled()
		}
		var cappedLifecycleTools []string
		for _, name := range lifecycleToolNames() {
			if cap := lifecycleToolCap(name); cap > 0 && toolUseCounts[name] >= cap {
				cappedLifecycleTools = append(cappedLifecycleTools, name)
			}
		}
		if len(cappedLifecycleTools) > 0 {
			iterationStrategy = iterationStrategy.WithHiddenTools(cappedLifecycleTools...)
		}
		var fullResp strings.Builder
		var nativeCalls []llm.ToolCall
		var streamErr error
		finishReason := ""
		var pendingStream strings.Builder
		suppressLegacyToolStream := false
		legacyToolSeen := false
		legacyToolReady := false
		nativeToolActivityAnnounced := false
		emitAgentActivity(eventCh, activityForIteration(i), "thinking", i)
		emitStream := func(content string) {
			if strings.TrimSpace(content) == "" || eventCh == nil {
				return
			}
			EmitAgentEvent(eventCh, AgentEvent{Type: "stream", Content: content})
		}
		handleStreamContent := func(content string) {
			fullResp.WriteString(content)
			if suppressLegacyToolStream {
				return
			}
			pendingStream.WriteString(content)
			pending := pendingStream.String()
			if idx := legacyToolMarkerIndex(pending); idx >= 0 {
				emitStream(pending[:idx])
				pendingStream.Reset()
				suppressLegacyToolStream = true
				if !legacyToolSeen {
					legacyToolSeen = true
					emitAgentActivity(eventCh, "Preparing tool call", "tool_selection", i)
				}
				return
			}
			if len(pending) > 96 {
				emit, keep := splitUTF8PrefixKeepTail(pending, 32)
				pendingStream.Reset()
				pendingStream.WriteString(keep)
				if emit != "" {
					emitStream(emit)
				}
			}
		}
		flushPendingStream := func() {
			if suppressLegacyToolStream {
				pendingStream.Reset()
				return
			}
			emitStream(pendingStream.String())
			pendingStream.Reset()
		}

		appendChatResponse := func(chatResp *llm.ChatResponse) {
			if chatResp.Content != "" {
				fullResp.WriteString(textutil.CleanUTF8(chatResp.Content))
			}
			if chatResp.FinishReason != "" {
				finishReason = chatResp.FinishReason
			}
			if chatResp.Usage.InputTokens != 0 || chatResp.Usage.OutputTokens != 0 {
				totalUsage.InputTokens += chatResp.Usage.InputTokens
				totalUsage.OutputTokens += chatResp.Usage.OutputTokens
				EmitAgentEvent(eventCh, AgentEvent{
					Type: "token.updated",
					Payload: map[string]interface{}{
						"input_tokens":  totalUsage.InputTokens,
						"output_tokens": totalUsage.OutputTokens,
					},
				})
			}
			if len(chatResp.ToolCalls) > 0 {
				nativeCalls = append(nativeCalls, chatResp.ToolCalls...)
			}
		}

		streamCtx, streamCancel := context.WithCancel(ctx)
		streamCh, err := a.streamChatWithRetry(streamCtx, messages, iterationStrategy)
		if err != nil {
			streamCancel()
			if ctx.Err() != nil {
				return "", totalUsage, fmt.Errorf("llm chat: %w", err)
			}
			fallbackResp, fallbackErr := a.chatResponseWithRetry(ctx, messages, iterationStrategy)
			if fallbackErr != nil {
				return "", totalUsage, fmt.Errorf("llm chat: %w; non-stream fallback failed: %v", err, fallbackErr)
			}
			appendChatResponse(fallbackResp)
		} else {
			streamStarted := time.Now()
			sawModelEvent := false
			waitTicker := time.NewTicker(2 * time.Second)
		streamLoop:
			for {
				select {
				case event, ok := <-streamCh:
					if !ok {
						break streamLoop
					}
					sawModelEvent = true
					if event.Err != nil {
						streamErr = event.Err
						break streamLoop
					}
					if event.EventType != "" && event.EventType != "stream" {
						emitProviderEvent(eventCh, event, i)
						if event.FinishReason != "" {
							finishReason = event.FinishReason
						}
						if event.Usage != nil {
							totalUsage.InputTokens += event.Usage.InputTokens
							totalUsage.OutputTokens += event.Usage.OutputTokens
							EmitAgentEvent(eventCh, AgentEvent{
								Type: "token.updated",
								Payload: map[string]interface{}{
									"input_tokens":  totalUsage.InputTokens,
									"output_tokens": totalUsage.OutputTokens,
								},
							})
						}
						if len(event.ToolCalls) > 0 {
							if !nativeToolActivityAnnounced {
								emitAgentActivity(eventCh, toolCallActivity(event.ToolCalls), "tool_selection", i)
								nativeToolActivityAnnounced = true
							}
							nativeCalls = append(nativeCalls, event.ToolCalls...)
						}
						continue
					}
					if event.Content != "" {
						handleStreamContent(textutil.CleanUTF8(event.Content))
					}
					if event.FinishReason != "" {
						finishReason = event.FinishReason
					}
					if event.Usage != nil {
						totalUsage.InputTokens += event.Usage.InputTokens
						totalUsage.OutputTokens += event.Usage.OutputTokens
						EmitAgentEvent(eventCh, AgentEvent{
							Type: "token.updated",
							Payload: map[string]interface{}{
								"input_tokens":  totalUsage.InputTokens,
								"output_tokens": totalUsage.OutputTokens,
							},
						})
					}
					if len(event.ToolCalls) > 0 {
						if !nativeToolActivityAnnounced {
							emitAgentActivity(eventCh, toolCallActivity(event.ToolCalls), "tool_selection", i)
							nativeToolActivityAnnounced = true
						}
						nativeCalls = append(nativeCalls, event.ToolCalls...)
					}
					if len(nativeCalls) == 0 && len(ExtractReadyToolCalls(fullResp.String())) > 0 {
						legacyToolReady = true
						streamCancel()
						break streamLoop
					}
				case <-waitTicker.C:
					emitAgentActivity(eventCh, modelWaitActivity(i, time.Since(streamStarted), sawModelEvent), "model_wait", i)
				case <-ctx.Done():
					streamErr = ctx.Err()
					break streamLoop
				}
			}
			waitTicker.Stop()
			streamCancel()

			if streamErr != nil {
				if legacyToolReady {
					streamErr = nil
				} else if ctx.Err() != nil {
					return "", totalUsage, fmt.Errorf("stream error: %w", streamErr)
				}
				if streamErr != nil && (fullResp.Len() > 0 || len(nativeCalls) > 0) {
					return "", totalUsage, fmt.Errorf("stream error after partial response: %w", streamErr)
				}
				if streamErr != nil {
					fallbackResp, fallbackErr := a.chatResponseWithRetry(ctx, messages, iterationStrategy)
					if fallbackErr != nil {
						return "", totalUsage, fmt.Errorf("stream error: %w; non-stream fallback failed: %v", streamErr, fallbackErr)
					}
					appendChatResponse(fallbackResp)
				}
			}
		}
		flushPendingStream()
		resp := textutil.CleanUTF8(fullResp.String())
		legacyMarkupPresent := legacyToolMarkerIndex(resp) >= 0
		nativeCalls = normalizeToolCallIDs(nativeCalls, i)
		calls, droppedForBudget := filterToolCallsByStrategyAndBudget(nativeCalls, iterationStrategy, actionToolsUsed)
		var droppedForLifecycle int
		calls, droppedForLifecycle = filterToolCallsByLifecycleCaps(calls, toolUseCounts)
		droppedForBudget += droppedForLifecycle
		if len(calls) == 0 {
			var legacyDropped int
			calls, legacyDropped = filterToolCallsByStrategyAndBudget(legacyToolCallsToLLM(ExtractToolCalls(resp), i), iterationStrategy, actionToolsUsed)
			var legacyLifecycleDropped int
			calls, legacyLifecycleDropped = filterToolCallsByLifecycleCaps(calls, toolUseCounts)
			droppedForBudget += legacyDropped
			droppedForBudget += legacyLifecycleDropped
		}
		if len(calls) == 0 && legacyMarkupPresent && droppedForBudget == 0 {
			droppedForBudget = 1
		}
		assistantContent := resp
		if len(calls) > 0 || droppedForBudget > 0 || legacyMarkupPresent {
			assistantContent = toolBudgetSafeAssistantContent(resp)
		}

		messages = append(messages, llm.Message{Role: "assistant", Content: assistantContent, ToolCalls: calls})
		history.Steps = append(history.Steps, assistantContent)

		// Sync turn to external memory providers after each assistant response
		a.syncTurn(ctx, tenantID, messages)

		// Turn-level lightweight fact extraction (frequency controlled)
		if a.turnExtractor != nil {
			turn := a.extractLastTurn(messages)
			if a.turnExtractor.ShouldExtract(turn, len(calls) > 0) {
				a.turnExtractor.Extract(ctx, tenantID, a.memory, turn)
				a.turnExtractor.ResetCounter()
			}
		}

		if len(calls) > 0 {
			if !nativeToolActivityAnnounced {
				emitAgentActivity(eventCh, toolCallActivity(calls), "tool_selection", i)
			}
			actionToolsUsed += countActionToolCalls(calls)
			incrementToolUseCounts(toolUseCounts, calls)
			results := a.executeToolCalls(ctx, tenantID, eventCh, calls)

			// Append results in order
			for _, res := range results {
				history.Steps = append(history.Steps, res.step)
				messages = append(messages, res.msg)
			}

			// Evolution review: 工具调用计数触发（非阻塞）
			a.toolCallCount += len(calls)
			// The review itself runs after the final answer, once the outcome is known.

			continue
		}
		if droppedForBudget > 0 && !toolBudgetRepairIssued {
			toolBudgetRepairIssued = true
			if i+1 >= maxIterations {
				maxIterations = i + 2
			}
			emitAgentActivity(eventCh, "Tool budget reached; finishing from collected evidence", "tool_budget", i)
			messages = append(messages, llm.Message{
				Role: "user",
				Content: "SelfMind tool budget for this turn has been reached. Do not call any more tools and do not output TOOL markup. " +
					"Write the final user-facing answer now from the evidence already collected. If the task cannot be fully completed, state the blocker and the exact next action in plain text.",
			})
			continue
		}
		if droppedForBudget > 0 {
			resp = toolBudgetSafeAssistantContent(resp)
			if strings.TrimSpace(resp) == "" {
				resp = "I reached the tool budget before I could complete the remaining tool step. Based on the evidence already collected, I should stop here and state the next action instead of calling more tools."
			}
			history.Steps[len(history.Steps)-1] = resp
			messages[len(messages)-1].Content = resp
		}

		if responseStoppedForOutputLimit(finishReason) {
			if resp != "" {
				continuedAnswer.WriteString(resp)
			}
			if i+1 < maxIterations {
				emitAgentActivity(eventCh, "Continuing because the model reached its output limit", "continuation", i)
				messages = append(messages, llm.Message{
					Role:    "user",
					Content: "Continue from the exact point where your previous answer stopped. Do not repeat earlier content. Finish the remaining answer completely.",
				})
				continue
			}
			history.Outcome = continuedAnswer.String()
			EmitAgentEvent(eventCh, AgentEvent{
				Type:    "turn.completed",
				Content: history.Outcome,
				Payload: map[string]interface{}{
					"status":        "incomplete",
					"finish_reason": finishReason,
				},
			})
			return history.Outcome, totalUsage, fmt.Errorf("model response reached the output limit before completing")
		}
		if continuedAnswer.Len() > 0 {
			continuedAnswer.WriteString(resp)
			resp = continuedAnswer.String()
			history.Steps[len(history.Steps)-1] = resp
			messages[len(messages)-1].Content = resp
		}

		// No tool calls — task complete
		history.Outcome = resp

		// Append this turn to the spine under the same key used to load it
		// (slim entry: user text + final answer + touched paths), and refresh
		// the task-coherent FTS index.
		a.saveHistory(ctx, tenantID, histKey, channel, initialPrompt, resp, messages)

		// Auto-extract durable facts from this conversation (async, non-blocking)
		if a.factExtractor != nil {
			go a.factExtractor.Extract(context.Background(), tenantID, a.memory, messages)
		}
		if a.profileSynthesizer != nil {
			// Gated internally; only makes a model call when enough new facts
			// have accumulated.
			go a.profileSynthesizer.MaybeSynthesize(context.Background(), tenantID, a.memory)
		}

		a.maybeTriggerBackgroundReview(tenantID, channel, messages, history)

		EmitAgentEvent(eventCh, AgentEvent{
			Type:    "turn.completed",
			Content: resp,
			Payload: map[string]interface{}{
				"status": "completed",
			},
		})
		return resp, totalUsage, nil
	}

	EmitAgentEvent(eventCh, AgentEvent{
		Type:    "turn.completed",
		Content: "max iterations reached",
		Payload: map[string]interface{}{
			"status": "max_iterations",
		},
	})
	return "max iterations reached", totalUsage, nil
}

func responseStoppedForOutputLimit(reason string) bool {
	reason = strings.ToLower(strings.TrimSpace(reason))
	switch reason {
	case "max_tokens", "length", "max_output_tokens", "output_limit":
		return true
	default:
		return strings.Contains(reason, "max_token") || strings.Contains(reason, "length")
	}
}

func splitUTF8PrefixKeepTail(s string, keepBytes int) (string, string) {
	if s == "" {
		return "", ""
	}
	if keepBytes <= 0 {
		return textutil.CleanUTF8(s), ""
	}
	if len(s) <= keepBytes {
		return "", s
	}
	cut := len(s) - keepBytes
	for cut > 0 && (!utf8.ValidString(s[:cut]) || !utf8.ValidString(s[cut:])) {
		cut--
	}
	if cut <= 0 {
		return "", s
	}
	return s[:cut], s[cut:]
}

func activityForIteration(iteration int) string {
	if iteration <= 0 {
		return "Thinking about the request"
	}
	return "Reading tool results and deciding the next step"
}

func modelWaitActivity(iteration int, elapsed time.Duration, sawModelEvent bool) string {
	seconds := int(elapsed.Round(time.Second).Seconds())
	if seconds < 1 {
		seconds = 1
	}
	if !sawModelEvent {
		if iteration <= 0 {
			return fmt.Sprintf("Waiting for the model to choose the first step (%ds)", seconds)
		}
		return fmt.Sprintf("Waiting for the model to decide after tool results (%ds)", seconds)
	}
	return fmt.Sprintf("Receiving the model response (%ds)", seconds)
}

func toolCallActivity(calls []llm.ToolCall) string {
	if len(calls) == 1 {
		name := strings.TrimSpace(calls[0].Function)
		if name == "" {
			name = "tool"
		}
		return "Preparing to run " + name
	}
	return fmt.Sprintf("Preparing to run %d tools", len(calls))
}

func emitAgentActivity(eventCh chan string, content, phase string, iteration int) {
	EmitAgentEvent(eventCh, AgentEvent{
		Type:    "agent.thinking",
		Content: content,
		Payload: map[string]interface{}{
			"phase":     phase,
			"iteration": iteration + 1,
		},
	})
}

func emitProviderEvent(eventCh chan string, event llm.StreamEvent, iteration int) {
	if eventCh == nil || strings.TrimSpace(event.EventType) == "" {
		return
	}
	payload := map[string]interface{}{}
	for k, v := range event.Payload {
		payload[k] = v
	}
	payload["iteration"] = iteration
	agentEvent := AgentEvent{
		Type:            event.EventType,
		Content:         textutil.CleanUTF8(event.Content),
		ToolName:        event.ToolName,
		ToolCallID:      event.ToolCallID,
		ToolArgs:        event.ToolArgs,
		ToolResult:      textutil.CleanUTF8(event.ToolResult),
		DurationSeconds: event.DurationSeconds,
		Payload:         payload,
	}
	if event.Err != nil {
		agentEvent.Error = event.Err.Error()
	}
	EmitAgentEvent(eventCh, agentEvent)
}

// triggerEvolutionReview 异步触发 evolution review（不阻塞主会话）
func (a *Agent) maybeTriggerBackgroundReview(tenantID, channel string, messages []llm.Message, history TaskHistory) {
	interval := a.nudgeInterval
	if interval <= 0 {
		interval = 10
	}
	a.turnReviewCount++
	reviewMemory := a.turnReviewCount >= interval
	reviewSkills := a.toolCallCount >= interval
	if !reviewMemory && !reviewSkills {
		return
	}
	if reviewMemory {
		a.turnReviewCount = 0
	}
	if reviewSkills {
		a.toolCallCount = 0
	}
	if a.ReviewEngine != nil {
		a.ReviewEngine.SpawnReview(tenantID, channel, messages, reviewMemory, reviewSkills)
		return
	}
	if reviewSkills && a.Reflector != nil {
		a.triggerEvolutionReview(tenantID, history)
	}
}

func (a *Agent) triggerEvolutionReview(tenantID string, history TaskHistory) {
	if a.Reflector == nil {
		return
	}

	// 深拷贝数据，避免竞态
	historyCopy := TaskHistory{
		Goal:    history.Goal,
		Context: history.Context,
		Outcome: history.Outcome,
		Steps:   append([]string{}, history.Steps...),
	}

	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		bgCtx = llm.WithModelContext(bgCtx, llm.ModelContext{
			TenantID: tenantID,
			Role:     llm.RoleSkillCurator,
		})

		result, err := a.Reflector.Reflect(bgCtx, historyCopy)
		if err != nil || result == nil || result.Action == "skip" {
			return
		}

		if err := a.Reflector.ArchiveSkill(bgCtx, result); err != nil {
			return
		}

		// 发送通知到 TUI
		if a.evolutionNotifyCh != nil && result.Action != "skip" {
			action := map[string]string{"create": "created", "update": "updated"}[result.Action]
			msg := fmt.Sprintf("💾 skill %s %s", result.SkillName, action)
			select {
			case a.evolutionNotifyCh <- msg:
			default:
			}
		}
	}()
}

func (a *Agent) selectRuntimeContext(ctx context.Context, tenantID, channel, prompt string, strategy TaskStrategy, eventCh chan string) context.Context {
	if _, ok := RuntimeContextBundleFromContext(ctx); ok {
		return ctx
	}
	bundle := RuntimeContextBundle{
		Channel: strings.TrimSpace(channel),
		Budget:  DefaultRuntimeContextBudget(),
	}
	if workspace, ok := WorkspaceContextFromContext(ctx); ok && strings.TrimSpace(workspace.Root) != "" {
		ws := workspace
		bundle.Workspace = &ws
		bundle.SelectionNotes = append(bundle.SelectionNotes, "active workspace selected from request context")
	}
	if runtime, ok := TaskRuntimeContextFromContext(ctx); ok {
		rt := runtime
		bundle.Task = &rt
		bundle.SelectionNotes = append(bundle.SelectionNotes, "active task/run slice selected from control event log")
	}

	if a.shouldRecallMemory(strategy, bundle) {
		for _, mem := range a.selectMemorySnippets(ctx, tenantID, prompt, bundle.Budget.MemoryChars) {
			bundle.Memories = append(bundle.Memories, mem)
		}
		if len(bundle.Memories) > 0 {
			bundle.SelectionNotes = append(bundle.SelectionNotes, fmt.Sprintf("%d indexed memory snippet(s) selected by query relevance", len(bundle.Memories)))
		}
	}
	if bundle.Empty() {
		return ctx
	}
	EmitAgentEvent(eventCh, AgentEvent{
		Type:    "context.selected",
		Content: strings.Join(bundle.SelectionNotes, "; "),
		Payload: map[string]interface{}{
			"channel":        bundle.Channel,
			"has_workspace":  bundle.Workspace != nil,
			"has_task":       bundle.Task != nil,
			"memory_count":   len(bundle.Memories),
			"budget_chars":   bundle.Budget.TotalChars,
			"workspace_root": workspaceRootForBundle(bundle),
		},
	})
	return WithRuntimeContextBundle(ctx, bundle)
}

func (a *Agent) shouldRecallMemory(strategy TaskStrategy, bundle RuntimeContextBundle) bool {
	if a == nil || a.memory == nil {
		return false
	}
	strategy = strategy.normalized()
	if strategy.ToolMode == ToolModeNone && bundle.Task == nil {
		return false
	}
	return true
}

func (a *Agent) selectMemorySnippets(ctx context.Context, tenantID, query string, maxChars int) []RuntimeMemoryContext {
	raw := a.autoRecallWithBudget(ctx, tenantID, query, maxChars)
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	lines := strings.Split(raw, "\n")
	out := make([]RuntimeMemoryContext, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "-"))
		if line == "" {
			continue
		}
		id := ""
		summary := line
		if strings.HasPrefix(line, "Session ") {
			rest := strings.TrimPrefix(line, "Session ")
			if before, after, ok := strings.Cut(rest, ":"); ok {
				id = strings.TrimSpace(before)
				summary = strings.TrimSpace(after)
			}
		}
		out = append(out, RuntimeMemoryContext{Source: "session", ID: id, Summary: summary})
		if len(out) >= 5 {
			break
		}
	}
	return out
}

func workspaceRootForBundle(bundle RuntimeContextBundle) string {
	if bundle.Workspace == nil {
		return ""
	}
	return bundle.Workspace.Root
}

func (a *Agent) autoRecall(ctx context.Context, tenantID, query string) string {
	if a.memory == nil {
		return ""
	}
	return a.autoRecallWithBudget(ctx, tenantID, query, 4000)
}

// autoRecallWithBudget searches historical sessions with a dynamic character budget.
// It retrieves up to 10 candidate sessions and includes as many as fit within maxChars.
func (a *Agent) autoRecallWithBudget(ctx context.Context, tenantID, query string, maxChars int) string {
	if a.memory == nil || maxChars <= 0 {
		return ""
	}

	// Semantic expansion: extend query with synonyms / related concepts
	searchQuery := query
	if a.semanticExpander != nil {
		searchQuery = a.semanticExpander.Expand(ctx, query)
	}

	// Build FTS5 OR query from expanded terms
	terms := strings.Fields(searchQuery)
	ftsQuery := ""
	if len(terms) > 0 {
		var parts []string
		for _, t := range terms {
			t = strings.ReplaceAll(t, `"`, `""`)
			if t != "" {
				parts = append(parts, fmt.Sprintf("content:%s* OR summary:%s*", t, t))
			}
		}
		ftsQuery = strings.Join(parts, " OR ")
	}
	if ftsQuery == "" {
		ftsQuery = query
	}

	// Query more candidates (up to 10), then filter by budget
	sessions, err := a.memory.SearchSessions(tenantID, ftsQuery, 10)
	if err != nil || len(sessions) == 0 {
		return ""
	}

	var sb strings.Builder
	used := 0
	for _, s := range sessions {
		snippet := s.Summary
		if snippet == "" {
			if len(s.Content) > 800 {
				snippet = textutil.TruncateBytes(s.Content, 800) + "..."
			} else {
				snippet = s.Content
			}
		}
		entry := fmt.Sprintf("- Session %s: %s\n", s.SessionID, snippet)
		if used+len(entry) > maxChars {
			break
		}
		sb.WriteString(entry)
		used += len(entry)
	}
	return sb.String()
}

// saveHistory persists this turn's durable working context under the SAME key
// BuildMessages loaded it from, and refreshes the FTS index.
//
// Spine turns persist ONE slim turn-level entry (user text + assistant final
// answer + touched file paths + source tag) — never the full messages array:
// tool intermediates and the system prompt stay in run events, not the spine.
// Internal-subsystem keys keep the legacy full-blob shape (their history is
// run-local scaffolding, not the person's work record).
func (a *Agent) saveHistory(ctx context.Context, tenantID, histKey, channel, userInput, finalAnswer string, messages []llm.Message) {
	if a.memory == nil {
		return
	}
	if histKey == SpineTrajectoryKey {
		entry := buildSpineEntry(ctx, userInput, finalAnswer, messages)
		if strings.TrimSpace(entry.User) != "" || strings.TrimSpace(entry.Assistant) != "" {
			if data, err := json.Marshal(entry); err == nil {
				a.memory.SaveTrajectory(ctx, tenantID, histKey, data)
			}
		}
	} else {
		record := struct {
			Messages []llm.Message `json:"messages"`
		}{Messages: messages}
		if data, err := json.Marshal(record); err == nil {
			a.memory.SaveTrajectory(ctx, tenantID, histKey, data)
		}
	}

	// Index under a task-derived session id when the turn is bound to a task, so
	// session_search retrieves the whole task ("what we did on the order system")
	// as ONE coherent session regardless of which endpoint the turns arrived on.
	// IndexSession is idempotent per session id, so re-indexing the growing task
	// trajectory each turn overwrites rather than fragments it. Taskless turns
	// keep the per-content session id. Indexing is keyed by the task-derived
	// session id and the REAL channel — never by the spine key.
	record := struct {
		Messages []llm.Message `json:"messages"`
	}{Messages: messages}
	data, err := json.Marshal(record)
	if err != nil {
		return
	}
	sessionID := a.sessionKey(ctx, messages)
	a.memory.IndexSession(ctx, tenantID, channel, sessionID, data)
}

// trajectoryKey resolves the storage partition for this turn's working-context
// history: the person-level WORK SPINE (docs/work-timeline.md). ALL of a
// person's agent-bound turns — task-bound, casual, cron — append to the one
// constant spine key; the storage tenant is already the person, so the key is
// person-scoped without colliding across people, and a task started on one
// endpoint continues from any other with the same recent context. Chat
// transcripts stay channel-local in control.channel_messages; the spine is the
// durable working-state layer, not a transcript mirror. Internal subsystem
// turns (delegation sub-agents, background review) never write the spine —
// they keep a channel-local bucket, per the work-timeline write rules.
func (a *Agent) trajectoryKey(ctx context.Context, channel string) string {
	if isInternalWorkChannel(channel) {
		return legacyChannelKey(channel)
	}
	return SpineTrajectoryKey
}

// trajectoryFallbackKeys returns the ordered READ-ONLY legacy compat chain
// consulted when the spine has nothing for this turn yet: the pre-spine
// `task:<id>` key first, then the task's prior run channel (where a task older
// than task-keyed history stored its transcript), or — for taskless turns —
// the old channel-derived key. saveHistory always writes the primary key, so
// history migrates forward on the first save.
func (a *Agent) trajectoryFallbackKeys(ctx context.Context, channel string) []string {
	if isInternalWorkChannel(channel) {
		return nil
	}
	if runtime, ok := TaskRuntimeContextFromContext(ctx); ok {
		var keys []string
		if id := strings.TrimSpace(runtime.TaskID); id != "" {
			keys = append(keys, "task:"+id)
		}
		if prior := strings.TrimSpace(runtime.PriorChannel); prior != "" {
			keys = append(keys, prior)
		}
		return keys
	}
	return []string{legacyChannelKey(channel)}
}

// isInternalWorkChannel reports whether the turn belongs to an internal
// subsystem, not the person's own work: delegation sub-agents and background
// review must never append to the person's spine (the parent run's turn is the
// spine record).
func isInternalWorkChannel(channel string) bool {
	channel = strings.TrimSpace(channel)
	return channel == "delegation" || strings.HasSuffix(channel, ":background_review")
}

// legacyChannelKey is the pre-spine channel-derived key: a bare per-session
// UUID (the in-process TUI mints one per launch) collapses to a stable
// per-person key so history is not scattered across restarts; stable channels
// keep their own bucket. It survives as the internal-subsystem key and the
// taskless leg of the legacy compat read chain.
func legacyChannelKey(channel string) string {
	channel = strings.TrimSpace(channel)
	if channel == "" || looksLikeSessionUUID(channel) {
		return "session"
	}
	return channel
}

// sessionKey ties a task's turns to one FTS session id so cross-endpoint recall
// spans the whole task; taskless turns fall back to a per-content id.
func (a *Agent) sessionKey(ctx context.Context, messages []llm.Message) string {
	if runtime, ok := TaskRuntimeContextFromContext(ctx); ok {
		if id := strings.TrimSpace(runtime.TaskID); id != "" {
			return "task:" + id
		}
	}
	return generateSessionID(messages)
}

// looksLikeSessionUUID reports whether s is a bare RFC-4122 UUID — the shape the
// in-process TUI mints once per launch. Such a channel is not a durable identity,
// so taskless history keyed by it would fragment across restarts.
func looksLikeSessionUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		r := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
			continue
		}
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}

// syncTurn persists the current turn to external memory providers.
// Called after every assistant response (including tool-calling turns).
func (a *Agent) syncTurn(ctx context.Context, tenantID string, messages []llm.Message) {
	if a.memory == nil || a.syncQueue == nil {
		return
	}
	// Serialize full messages for providers that need complete context
	record := struct {
		Messages []llm.Message `json:"messages"`
	}{Messages: messages}
	data, err := json.Marshal(record)
	if err != nil {
		return
	}
	req := syncTurnRequest{tenantID: tenantID, data: data}
	select {
	case a.syncQueue <- req:
	default:
		select {
		case <-a.syncQueue:
		default:
		}
		select {
		case a.syncQueue <- req:
		default:
		}
	}
}

func (a *Agent) runSyncWorker() {
	for req := range a.syncQueue {
		if a.memory == nil {
			continue
		}
		bgCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		a.memory.SyncMessagesAll(bgCtx, req.tenantID, req.data)
		cancel()
	}
}

// extractLastTurn extracts the most recent user-assistant pair from messages.
// If tool results exist between user and assistant, they are included in the turn context.
func (a *Agent) extractLastTurn(messages []llm.Message) memory.MessagePair {
	turn := memory.MessagePair{}
	// Find the last assistant message
	lastAssistantIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" {
			lastAssistantIdx = i
			turn.Assistant = messages[i].Content
			break
		}
	}
	if lastAssistantIdx < 0 {
		return turn
	}
	// Find the user message that preceded this assistant response
	for i := lastAssistantIdx - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			turn.User = messages[i].Content
			break
		}
	}
	return turn
}

func generateSessionID(messages []llm.Message) string {
	for _, m := range messages {
		if m.Role == "user" {
			content := m.Content
			if len(content) > 64 {
				content = textutil.TruncateBytes(content, 64)
			}
			sum := sha256Hash(content)
			return sum[:16]
		}
	}
	return fmt.Sprintf("sess-%d", len(messages))
}

func sha256Hash(s string) string {
	h := sha256.New()
	h.Write([]byte(s))
	return fmt.Sprintf("%x", h.Sum(nil))
}

func (a *Agent) BuildSystemPrompt(ctx context.Context, tenantID string) (string, error) {
	return a.buildSystemPrompt(ctx, tenantID, DefaultTaskStrategy(), "")
}

func (a *Agent) buildSystemPrompt(ctx context.Context, tenantID string, strategy TaskStrategy, userInput string) (string, error) {
	var parts []string

	// 1. Core Persona (Soul)
	if a.soul != "" {
		parts = append(parts, a.soul)
	}
	parts = append(parts, selfImprovementGuidance())

	if bundle, ok := RuntimeContextBundleFromContext(ctx); ok {
		if prompt := bundle.Prompt(8000); strings.TrimSpace(prompt) != "" {
			parts = append(parts, prompt)
		}
	} else {
		if workspace, ok := WorkspaceContextFromContext(ctx); ok {
			var sb strings.Builder
			sb.WriteString("# CURRENT WORKSPACE\n")
			if workspace.ID != "" {
				sb.WriteString(fmt.Sprintf("workspace_id: %s\n", workspace.ID))
			}
			sb.WriteString(fmt.Sprintf("workspace_root: %s\n", workspace.Root))
			sb.WriteString("Use workspace_root as the default directory for local tools and relative paths.\n")
			sb.WriteString("When the user asks about this project, current repo, current codebase, or names a project without a path, inspect workspace_root first.\n")
			sb.WriteString("If the request arrives from IM and no workspace is available, ask the user to select or bind a workspace instead of guessing a local path.")
			parts = append(parts, sb.String())
		}

		if runtime, ok := TaskRuntimeContextFromContext(ctx); ok {
			if prompt := runtime.Prompt(6000); strings.TrimSpace(prompt) != "" {
				parts = append(parts, prompt)
			}
		}
	}

	// 2. Tool Instructions - 增强指令强度
	if a.backend != nil {
		defs := filterToolDefinitions(a.backend.GetToolDefinitions(), strategy)
		if len(defs) > 0 {
			var sb strings.Builder
			sb.WriteString("\n# TOOL USE INSTRUCTIONS\n")
			sb.WriteString("Use local tools whenever the user asks about local files, directories, command output, project state, or system status.\n")
			sb.WriteString("Prefer native tool calls when the model interface supports them. If native tool calls are unavailable, use the exact fallback format: [TOOL:tool_name:{\"arg\": \"val\"}]\n")
			sb.WriteString("Do not emit XML-style tool tags such as <tool> or <parameter>; they are only tolerated for compatibility and should not appear in user-facing answers.\n")
			sb.WriteString("When a tool returns an error, treat the error as diagnostic evidence. Do not stop at the first failed command unless the failure is the requested final result. Inspect cwd, files, environment, auth state, provider constraints, or command help as needed, then choose the next correct action.\n")
			sb.WriteString("Do not hard-code environment overrides as a default tool behavior. For example, if Go reports a go.work/module boundary error, first inspect go env GOWORK/GOMOD and the relevant go.work/go.mod files, then decide whether to change cwd, use an explicit env override, or report a real blocker.\n")
			sb.WriteString("Use update_plan only for non-trivial work: tasks with 3+ meaningful steps, multi-file changes, investigation/debugging, long-running verification, or explicit user requests for a plan. Do not use update_plan for one-shot answers, small code examples, simple commands, or direct explanations.\n")
			sb.WriteString("For non-trivial tool-using work that creates or changes durable task state, call finish_run once with a structured outcome: status, summary, done, next_steps, files, tests, risks, and need_approve. Skip finish_run for direct answers, small snippets, and ordinary explanations.\n")
			sb.WriteString("Use tool_search when you need a capability but are unsure which registered tool fits.\n")
			sb.WriteString("The ONLY valid tool names are: ")
			for i, d := range defs {
				sb.WriteString(fmt.Sprintf("'%s'", toolDefinitionName(d)))
				if i < len(defs)-1 {
					sb.WriteString(", ")
				}
			}
			sb.WriteString(".\n")
			sb.WriteString("DO NOT use tools like 'ls', 'cat', 'read', 'run_command', or 'sh' which do not exist. Use the specific tools listed above.\n")
			sb.WriteString("Do not invent tool names. If you use the fallback tag format, output only the [TOOL:...] tag for that step.\n\n")
			sb.WriteString("## Available Tools\n")
			for _, d := range defs {
				sb.WriteString(fmt.Sprintf("### %s\n%s\n", toolDefinitionName(d), toolDefinitionDescription(d)))
				if params := toolDefinitionParameters(d); params != nil {
					if props, ok := params["properties"].(map[string]interface{}); ok {
						sb.WriteString("Parameters:\n")
						for pName, pDef := range props {
							if def, ok := pDef.(map[string]interface{}); ok {
								sb.WriteString(fmt.Sprintf("- %s (%s): %s\n", pName, def["type"], def["description"]))
							}
						}
					}
				}
				sb.WriteString("\n")
			}
			parts = append(parts, sb.String())

			// Work-quality discipline (explore, prefer patch, verify) applies to
			// all tool-bearing turns. Frontend/UI design guidance is added only
			// when the task actually looks like UI work, so backend/data/CLI
			// tasks are not biased toward frontend concerns.
			parts = append(parts, taskExecutionGuidance())
			parts = append(parts, progressNarrationGuidance())
			if isFrontendTask(userInput) {
				parts = append(parts, frontendQualityGuidance())
			}

			// Surface learned skills so the agent applies what it already knows
			// (only on tool-bearing turns, where skill_view is usable).
			if a.skillInventory != nil {
				if block := strings.TrimSpace(a.skillInventory(tenantID)); block != "" {
					parts = append(parts, block)
				}
			}
		}
	}

	if a.memory == nil {
		return strings.Join(parts, "\n\n"), nil
	}

	userFacts, _ := a.memory.GetFacts(ctx, tenantID, "user")
	memFacts, _ := a.memory.GetFacts(ctx, tenantID, "memory")

	// Synthesized user profile (a coherent understanding distilled from many
	// facts) is the primary "knows you" signal; it is shorter than dumping every
	// raw fact and reads as a stable picture of the user.
	if summary := a.latestProfileSummary(ctx, tenantID); summary != "" {
		parts = append(parts, "<user-profile>\n[System note: synthesized understanding of the user from past interactions; background context, not new input.]\n"+summary+"\n</user-profile>")
	}

	// Select the most relevant facts (W3d): rank by decayed confidence × scope
	// relevance instead of plain recency, so high-trust and on-workspace facts
	// win the bounded slot. Legacy facts (zero metadata) score neutrally, so
	// they are not dropped.
	const maxFactsEach = 20
	currentScope := "global"
	if ws, ok := WorkspaceContextFromContext(ctx); ok && ws.ID != "" {
		currentScope = "workspace:" + ws.ID
	}
	now := time.Now()
	userFacts = memory.SelectFacts(userFacts, currentScope, now, maxFactsEach)
	memFacts = memory.SelectFacts(memFacts, currentScope, now, maxFactsEach)

	if len(userFacts) > 0 || len(memFacts) > 0 {
		var factBlock strings.Builder
		if a.useMemoryFence {
			factBlock.WriteString("<memory-context>\n[System note: The following is recalled memory context, NOT new user input. Treat as informational background data.]\n\n")
			factBlock.WriteString("## User Preferences\n")
			for _, f := range userFacts {
				factBlock.WriteString(fmt.Sprintf("- %s\n", f.Content))
			}
			if len(memFacts) > 0 {
				factBlock.WriteString("\n## Environment Facts\n")
				for _, f := range memFacts {
					factBlock.WriteString(fmt.Sprintf("- %s\n", f.Content))
				}
			}
			factBlock.WriteString("</memory-context>")
		} else {
			factBlock.WriteString("<MEMORY>\n")
			for _, f := range userFacts {
				factBlock.WriteString(fmt.Sprintf("- [User Preference]: %s\n", f.Content))
			}
			for _, f := range memFacts {
				factBlock.WriteString(fmt.Sprintf("- [Environment]: %s\n", f.Content))
			}
			factBlock.WriteString("</MEMORY>")
		}
		parts = append(parts, factBlock.String())
	}

	return strings.Join(parts, "\n\n"), nil
}

func filterToolDefinitions(defs []map[string]interface{}, strategy TaskStrategy) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(defs))
	for _, def := range defs {
		name := toolDefinitionName(def)
		if !strategy.AllowsTool(name) {
			continue
		}
		out = append(out, def)
	}
	return out
}

func toolBudgetSafeAssistantContent(content string) string {
	cleaned := strings.TrimSpace(StripLegacyToolMarkup(content))
	if cleaned != "" {
		return cleaned
	}
	return ""
}
