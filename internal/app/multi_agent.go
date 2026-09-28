package app

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/promptassets"
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
// Each subagent runs in its own goroutine and, like single-goal delegation,
// keeps no conversation memory and acts as the parent run's person.
type MultiAgentHost struct {
	backend       kernel.AgentBackend // Parent backend for subagent tool access
	provider      llm.Provider        // LLM provider for subagents
	prompts       *promptassets.Snapshot
	maxConcurrent int // Max parallel subagents (semaphore)
	maxDepth      int // Max delegation depth (prevent runaway recursion)
	maxIterations int // Max iterations per subagent
	maxRetries    int // Provider retries per subagent call
	stopCh        chan struct{}
	mu            sync.Mutex
	running       map[string]context.CancelFunc // taskID -> cancel func

	// subBackendBuilder, when set, overrides the default toolset-filtering
	// backend construction. Delegation uses it to hand sub-agents a
	// depth-bounded backend (delegate_task stripped past the budget) so a batch
	// sub-agent cannot recurse any more than a single-goal one can.
	subBackendBuilder func(toolsets []string) kernel.AgentBackend
}

// SetSubBackendBuilder installs a custom sub-agent backend builder. It must be
// called before RunBatch. See buildDelegateSubBackend for the depth-bounding
// contract.
func (h *MultiAgentHost) SetSubBackendBuilder(fn func(toolsets []string) kernel.AgentBackend) {
	h.subBackendBuilder = fn
}

// NewMultiAgentHost creates a new MultiAgentHost.
func NewMultiAgentHost(
	backend kernel.AgentBackend,
	provider llm.Provider,
	prompts *promptassets.Snapshot,
	maxConcurrent, maxDepth, maxIterations, maxRetries int,
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
	if maxRetries <= 0 {
		maxRetries = 3
	}
	return &MultiAgentHost{
		backend:       backend,
		provider:      provider,
		prompts:       prompts,
		maxConcurrent: maxConcurrent,
		maxDepth:      maxDepth,
		maxIterations: maxIterations,
		maxRetries:    maxRetries,
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
		// Each goal is its own sub-execution: siblings restart their call IDs
		// too, so each needs its own name.
		namespace := taskID
		if parent := kernel.DelegationNamespace(ctx); parent != "" {
			namespace = fmt.Sprintf("%s-%d", parent, i)
		}
		taskCtx = kernel.WithDelegationNamespace(taskCtx, namespace)
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
	// Build subagent backend with toolset restrictions. A delegation-supplied
	// builder (depth-bounded, delegate_task stripped past budget) takes
	// precedence over the default toolset filter.
	var subBackend kernel.AgentBackend
	if h.subBackendBuilder != nil {
		subBackend = h.subBackendBuilder(task.Toolsets)
	} else {
		subBackend = h.buildSubBackend(task.Toolsets)
	}

	resp, usage, err := runDelegatedGoal(ctx, subBackend, h.provider, h.prompts, h.maxIterations, h.maxRetries, delegatedTaskPrompt(task.Goal, task.Context, task.Toolsets))
	if err != nil {
		return resp, usage, fmt.Errorf("subagent %s: %w", taskID, err)
	}
	return resp, usage, nil
}

// buildSubBackend returns a backend filtered to the requested toolsets. With no
// explicit toolsets it clones the ordinary parent capabilities while still
// excluding parent-owned lifecycle and durable-learning tools.
func (h *MultiAgentHost) buildSubBackend(toolsets []string) kernel.AgentBackend {
	// Try to get the Dispatcher to build a filtered registry
	disp, ok := h.backend.(*tools.Dispatcher)
	if !ok {
		// Backend is not a Dispatcher; fall back to full backend
		return h.backend
	}

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

	// The subset keeps the parent's policy chain, as in single-goal delegation.
	return disp.Subset(func(name string) bool {
		if name == "delegate_task" || parentOwnedDelegationTool(name) {
			return false
		}
		return requestedTools[name] || len(toolsets) == 0
	})
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
