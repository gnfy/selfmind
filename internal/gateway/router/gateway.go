package router

import (
	"context"
	"fmt"
	"runtime/debug"
	"strings"

	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
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

// Gateway is the agent-execution facade the daemon's httpapi orchestration
// runs turns through: intent classification, the worker pool, event bridging,
// and management-tool dispatch. It has no routing or task authority of its
// own — identity, tasks, runs, and delivery live in gateway/httpapi +
// control.db (the raw-inbound Handle path and its IM adapters were removed
// with the legacy tasks.db manager).
type Gateway struct {
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

func (g *Gateway) RuntimeContextBudget() kernel.RuntimeContextBudget {
	if g == nil || g.agent == nil {
		return kernel.DefaultRuntimeContextBudget()
	}
	return g.agent.RuntimeContextBudget()
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
	serialPaths := workspaceSerialPaths(ctx)
	serialKey := workspaceSerialKey(ctx)
	observe := func(state runpool.State) {
		payload := map[string]interface{}{"state": string(state)}
		if len(serialPaths) > 0 {
			payload["resource"] = fmt.Sprintf("filesystem roots (%d)", len(serialPaths))
		} else if serialKey != "" {
			payload["resource"] = "workspace:" + serialKey
		}
		kernel.EmitAgentEvent(kernel.EventChannelFromContext(ctx), kernel.AgentEvent{
			Type:    "run.scheduler",
			Content: schedulerStateMessage(state),
			Payload: payload,
		})
	}
	run := func() error {
		ag := <-g.agents
		defer func() { g.agents <- ag }()
		resp, usage, runErr = ag.RunConversation(ctx, uid, channel, input)
		return runErr
	}
	var poolErr error
	if len(serialPaths) > 0 {
		poolErr = g.pool.RunObservedPaths(ctx, serialPaths, observe, run)
	} else {
		poolErr = g.pool.RunObserved(ctx, serialKey, observe, run)
	}
	if poolErr != nil && runErr == nil {
		// Cancelled while queued (or pool returned before running fn).
		return "", usage, poolErr
	}
	return resp, usage, runErr
}

func schedulerStateMessage(state runpool.State) string {
	switch state {
	case runpool.StateWaitingResource:
		return "Waiting for the workspace write lock."
	case runpool.StateWaitingWorker:
		return "Waiting for an agent worker."
	case runpool.StateRunning:
		return "Agent worker acquired."
	default:
		return "Agent scheduler state changed."
	}
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

func workspaceSerialPaths(ctx context.Context) []string {
	ws, ok := kernel.WorkspaceContextFromContext(ctx)
	if !ok {
		return nil
	}
	if strategy, ok := kernel.TaskStrategyFromContext(ctx); ok && !strategy.MayWriteWorkspace() {
		return nil
	}
	return ws.ContextRoots()
}

func NewGateway(agent *kernel.Agent, llmProvider llm.Provider) *Gateway {
	return &Gateway{
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
		return "I am SelfMind. No usable AI model configuration was resolved. Run `selfmind model`; selections are checked automatically."
	}
	return fmt.Sprintf("I am SelfMind. The current Main model is %s. Change Main, Background, or optional role overrides with `selfmind model`; SelfMind validates the draft and schedules one safe restart.", label)
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
