package app

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/tools"
)

// Task represents a single subagent task in a batch.
type Task struct {
	Goal     string   // The goal/prompt for this subagent
	Context  string   // Additional context to prepend to the goal
	Toolsets []string // Toolset names to restrict subagent capabilities (e.g. ["file", "web"])
	ID       string   // Optional task ID; auto-generated if empty
}

// Result holds the outcome of a single subagent task.
type Result struct {
	TaskID   string
	Response string
	Usage    llm.UsageStats
	Error    error
}

// MultiAgentHost manages a pool of subagents for parallel task execution.
// Each subagent runs in its own goroutine with its own tenant ID for memory isolation.
type MultiAgentHost struct {
	backend       kernel.AgentBackend   // Parent backend for subagent tool access
	provider      llm.Provider          // LLM provider for subagents
	mem           *memory.MemoryManager // Shared memory manager; subagents get isolated tenantIDs
	maxConcurrent int                   // Max parallel subagents (semaphore)
	maxDepth      int                   // Max delegation depth (prevent runaway recursion)
	maxIterations int                   // Max iterations per subagent
	soul          string                // System prompt for subagents
	stopCh        chan struct{}
	mu            sync.Mutex
	running       map[string]context.CancelFunc // taskID -> cancel func
}

// NewMultiAgentHost creates a new MultiAgentHost.
func NewMultiAgentHost(
	backend kernel.AgentBackend,
	provider llm.Provider,
	mem *memory.MemoryManager,
	maxConcurrent, maxDepth, maxIterations int,
) *MultiAgentHost {
	if maxConcurrent <= 0 {
		maxConcurrent = 5
	}
	if maxDepth <= 0 {
		maxDepth = 2
	}
	if maxIterations <= 0 {
		maxIterations = 50
	}
	return &MultiAgentHost{
		backend:       backend,
		provider:      provider,
		mem:           mem,
		maxConcurrent: maxConcurrent,
		maxDepth:      maxDepth,
		maxIterations: maxIterations,
		soul:          "You are a specialized sub-agent helping with a task. Be concise and focused.",
		stopCh:        make(chan struct{}),
		running:       make(map[string]context.CancelFunc),
	}
}

// Stop cancels all running subagent tasks and halts the host.
func (h *MultiAgentHost) Stop() {
	close(h.stopCh)
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, cancel := range h.running {
		cancel()
	}
}

// RunBatch executes multiple tasks in parallel, respecting maxConcurrent.
// It returns results in the same order as the input tasks.
// If context is cancelled, all running tasks are cancelled and the function returns.
func (h *MultiAgentHost) RunBatch(ctx context.Context, tasks []Task) []Result {
	if ctx == nil {
		ctx = context.Background()
	}

	sem := make(chan struct{}, h.maxConcurrent)
	var wg sync.WaitGroup
	results := make([]Result, len(tasks))

	for i, task := range tasks {
		taskID := task.ID
		if taskID == "" {
			taskID = fmt.Sprintf("task-%s", uuid.New().String()[:8])
		}

		taskCtx, cancel := context.WithCancel(ctx)
		h.mu.Lock()
		h.running[taskID] = cancel
		h.mu.Unlock()

		wg.Add(1)
		go func(idx int, t Task, id string) {
			defer wg.Done()
			defer func() {
				cancel()
				h.mu.Lock()
				delete(h.running, id)
				h.mu.Unlock()
			}()

			select {
			case <-h.stopCh:
				results[idx] = Result{TaskID: id, Error: fmt.Errorf("host stopped")}
				return
			case sem <- struct{}{}:
				defer func() { <-sem }()
			}

			resp, usage, err := h.runSubAgent(taskCtx, t, id)
			results[idx] = Result{
				TaskID:   id,
				Response: resp,
				Usage:    usage,
				Error:    err,
			}
		}(i, task, taskID)
	}

	wg.Wait()
	return results
}

// runSubAgent creates a subagent, runs it, and returns the result.
func (h *MultiAgentHost) runSubAgent(ctx context.Context, task Task, taskID string) (string, llm.UsageStats, error) {
	// Each subagent gets its own isolated tenant ID
	subTenantID := fmt.Sprintf("subagent-%s", taskID)

	// Build subagent backend with toolset restrictions
	subBackend := h.buildSubBackend(task.Toolsets)

	// Build the full prompt
	fullPrompt := task.Goal
	if task.Context != "" {
		fullPrompt = fmt.Sprintf("Context:\n%s\n\nGoal:\n%s", task.Context, task.Goal)
	}

	// Create subagent
	subAgent := kernel.NewAgent(
		h.mem,
		subBackend,
		h.provider,
		h.soul,
		h.maxIterations,
		3, // maxRetries
		nil, // no reflector for subagents
	)

	resp, usage, err := subAgent.RunConversation(ctx, subTenantID, "delegation", fullPrompt)
	if err != nil {
		return resp, usage, fmt.Errorf("subagent %s: %w", taskID, err)
	}
	return resp, usage, nil
}

// buildSubBackend returns a backend filtered to only the requested toolsets.
// If no toolsets are specified, returns the full parent backend.
func (h *MultiAgentHost) buildSubBackend(toolsets []string) kernel.AgentBackend {
	if len(toolsets) == 0 {
		return h.backend
	}

	// Try to get the Dispatcher to build a filtered registry
	disp, ok := h.backend.(*tools.Dispatcher)
	if !ok {
		// Backend is not a Dispatcher; fall back to full backend
		return h.backend
	}

	subRegistry := tools.NewRegistry()
	allToolNames := disp.ListTools()

	requestedTools := make(map[string]bool)
	for _, ts := range toolsets {
		ts = normalizeToolset(ts)
		switch ts {
		case "file":
			requestedTools["read_file"] = true
			requestedTools["write_file"] = true
			requestedTools["patch"] = true
		case "terminal":
			requestedTools["terminal"] = true
			requestedTools["execute_code"] = true
		case "web":
			requestedTools["web_search"] = true
			requestedTools["web_extract"] = true
		case "memory":
			requestedTools["session_search"] = true
			requestedTools["memory"] = true
		case "skill":
			for _, name := range allToolNames {
				if len(name) > 6 && name[:6] == "skill:" {
					requestedTools[name] = true
				}
			}
		default:
			requestedTools[ts] = true
		}
	}

	for _, name := range allToolNames {
		if requestedTools[name] {
			if t, ok := disp.GetTool(name); ok {
				subRegistry.Register(t)
			}
		}
	}

	return tools.NewDispatcherWithRegistry(subRegistry)
}

// normalizeToolset normalizes common toolset aliases.
func normalizeToolset(ts string) string {
	switch ts {
	case "shell", "bash", "exec":
		return "terminal"
	case "search", "grep", "find":
		return "file"
	case "browser", "crawl":
		return "web"
	default:
		return ts
	}
}
