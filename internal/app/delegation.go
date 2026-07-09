package app

import (
	"context"
	"fmt"
	"strings"

	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/config"
	"selfmind/internal/tools"
)

// Delegation bound defaults. See config.DelegationConfig for the rationale;
// depth is a hard structural bound because tool execution has no context
// channel to carry a runtime depth counter, so nesting is enforced by
// controlling which tools a sub-agent's backend contains.
const (
	defaultDelegationMaxDepth      = 1
	defaultDelegationMaxConcurrent = 5
	defaultDelegationMaxSubtasks   = 16
)

const delegateSubAgentSoul = "You are a specialized sub-agent helping with a task."

// delegationLimits resolves configured bounds, applying safe defaults for any
// unset (zero) field.
func delegationLimits(cfg config.DelegationConfig) (maxDepth, maxConcurrent, maxSubtasks, maxIter, maxRetries int) {
	maxDepth = cfg.MaxDepth
	if maxDepth <= 0 {
		maxDepth = defaultDelegationMaxDepth
	}
	maxConcurrent = cfg.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = defaultDelegationMaxConcurrent
	}
	maxSubtasks = cfg.MaxSubtasks
	if maxSubtasks <= 0 {
		maxSubtasks = defaultDelegationMaxSubtasks
	}
	maxRetries = cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}
	maxIter = cfg.MaxIterations
	if maxIter <= 0 {
		maxIter = 50
	}
	return
}

// MakeDelegateFn returns a delegate function configured from config. The
// returned function runs at delegation depth 1 (the top-level agent's first
// hop); nested delegation is bounded by cfg.MaxDepth.
func MakeDelegateFn(mem *memory.MemoryManager, backend kernel.AgentBackend, cfg config.DelegationConfig) func(goal, contextStr string, toolsets []string) (string, llm.UsageStats, error) {
	return makeDelegateFnAtDepth(mem, backend, cfg, 1)
}

// makeDelegateFnAtDepth builds the single-goal delegate function for a given
// nesting depth. depth 1 is the top-level agent delegating; a sub-agent that is
// still allowed to delegate receives a fn built at depth+1.
func makeDelegateFnAtDepth(mem *memory.MemoryManager, backend kernel.AgentBackend, cfg config.DelegationConfig, depth int) func(goal, contextStr string, toolsets []string) (string, llm.UsageStats, error) {
	maxDepth, _, _, maxIter, maxRetries := delegationLimits(cfg)
	return func(goal, contextStr string, toolsets []string) (string, llm.UsageStats, error) {
		if depth > maxDepth {
			// Defensive: a leaf sub-agent should never hold this fn (its backend
			// has no delegate_task), but if wiring ever regresses, fail loudly
			// instead of recursing.
			return "", llm.UsageStats{}, fmt.Errorf("delegation depth limit reached (max %d); sub-agent cannot delegate further", maxDepth)
		}
		provider, err := makeDelegationProvider(cfg)
		if err != nil {
			return "", llm.UsageStats{}, err
		}

		subBackend := buildDelegateSubBackend(mem, backend, cfg, toolsets, depth)
		subAgent := kernel.NewAgent(mem, subBackend, provider, delegateSubAgentSoul, maxIter, maxRetries, nil)

		fullPrompt := fmt.Sprintf("Target Goal: %s\nContext: %s\nAvailable Toolsets: %v\n\nPlease complete the task and return the final result.", goal, contextStr, toolsets)

		// Execute in a sub-context
		return subAgent.RunConversation(context.Background(), "system", "delegation", fullPrompt)
	}
}

// buildDelegateSubBackend builds a fresh, bounded backend for a sub-agent at the
// given depth. It NEVER hands out the shared parent dispatcher: it always clones
// a filtered registry so the parent's delegate_task wiring cannot be mutated and
// so the sub-agent's delegation budget is controlled here.
//
// The delegate_task tool is stripped by default; it is re-added (wired to a
// depth+1 delegate fn) only while depth < maxDepth. At depth == maxDepth the
// sub-agent is a leaf with no delegation tool — the hard recursion bound.
func buildDelegateSubBackend(mem *memory.MemoryManager, backend kernel.AgentBackend, cfg config.DelegationConfig, toolsets []string, depth int) kernel.AgentBackend {
	maxDepth, _, _, _, _ := delegationLimits(cfg)

	disp, ok := backend.(*tools.Dispatcher)
	if !ok {
		// Non-dispatcher backends can't be filtered; return as-is. These do not
		// carry delegate_task, so there is no recursion mine to defuse.
		return backend
	}

	subRegistry := tools.NewRegistry()
	allTools := disp.ListTools()

	// Decide which parent tools to copy. Empty toolsets => copy everything
	// (preserving prior behavior), otherwise map toolset names to tools.
	var want map[string]bool
	if len(toolsets) > 0 {
		want = make(map[string]bool)
		for _, ts := range toolsets {
			ts = strings.TrimSpace(ts)
			switch ts {
			case "file":
				want["read_file"] = true
				want["write_file"] = true
				want["ls_r"] = true
				want["search_files"] = true
				want["patch"] = true
			case "terminal", "shell":
				want["terminal"] = true
			case "web":
				want["web_search"] = true
				want["web_extract"] = true
			default:
				want[ts] = true
			}
		}
	}

	for _, name := range allTools {
		// delegate_task is never copied from the parent; it is re-added below
		// only when the depth budget allows, so leaf sub-agents cannot recurse.
		if name == "delegate_task" {
			continue
		}
		if want != nil && !want[name] {
			continue
		}
		if t, ok := disp.GetTool(name); ok {
			subRegistry.Register(t)
		}
	}

	if depth < maxDepth {
		nested := tools.NewDelegateTool()
		nested.RegisterDelegateFn(makeDelegateFnAtDepth(mem, backend, cfg, depth+1))
		nested.RegisterBatchDelegateFn(makeDelegateBatchFnAtDepth(mem, backend, cfg, depth+1))
		subRegistry.Register(nested)
	}

	return tools.NewDispatcherWithRegistry(subRegistry)
}

func MakeDelegateBatchFn(mem *memory.MemoryManager, backend kernel.AgentBackend, cfg config.DelegationConfig) func(tasks []tools.DelegateTaskSpec) ([]tools.DelegateTaskResult, error) {
	return makeDelegateBatchFnAtDepth(mem, backend, cfg, 1)
}

func makeDelegateBatchFnAtDepth(mem *memory.MemoryManager, backend kernel.AgentBackend, cfg config.DelegationConfig, depth int) func(tasks []tools.DelegateTaskSpec) ([]tools.DelegateTaskResult, error) {
	maxDepth, maxConcurrent, maxSubtasks, maxIter, _ := delegationLimits(cfg)
	return func(specs []tools.DelegateTaskSpec) ([]tools.DelegateTaskResult, error) {
		if depth > maxDepth {
			return nil, fmt.Errorf("delegation depth limit reached (max %d); sub-agent cannot delegate further", maxDepth)
		}
		if len(specs) > maxSubtasks {
			return nil, fmt.Errorf("delegation batch too large: %d goals exceeds max_subtasks=%d", len(specs), maxSubtasks)
		}
		provider, err := makeDelegationProvider(cfg)
		if err != nil {
			return nil, err
		}
		host := NewMultiAgentHost(backend, provider, mem, maxConcurrent, maxDepth, maxIter)
		// Sub-agents in the batch get the same bounded backend as single-goal
		// delegation: filtered by toolsets, delegate_task stripped unless the
		// depth budget allows a depth+1 hop.
		host.SetSubBackendBuilder(func(toolsets []string) kernel.AgentBackend {
			return buildDelegateSubBackend(mem, backend, cfg, toolsets, depth)
		})
		defer host.Stop()

		batch := make([]Task, 0, len(specs))
		for _, spec := range specs {
			batch = append(batch, Task{
				Goal:     spec.Goal,
				Context:  spec.Context,
				Toolsets: spec.Toolsets,
			})
		}
		results := host.RunBatch(context.Background(), batch)
		out := make([]tools.DelegateTaskResult, 0, len(results))
		for i, result := range results {
			item := tools.DelegateTaskResult{
				Response: result.Response,
				Usage:    result.Usage,
			}
			if i < len(specs) {
				item.Goal = specs[i].Goal
			}
			if result.Error != nil {
				item.Error = result.Error.Error()
			}
			out = append(out, item)
		}
		return out, nil
	}
}

func makeDelegationProvider(cfg config.DelegationConfig) (llm.Provider, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("delegation API key not configured")
	}
	switch cfg.Provider {
	case "anthropic":
		provider := llm.NewAnthropicAdapter(cfg.APIKey)
		if cfg.Model != "" {
			provider.Model = cfg.Model
		}
		return provider, nil
	case "openai":
		provider := llm.NewOpenAIAdapter(cfg.APIKey)
		if cfg.Model != "" {
			provider.Model = cfg.Model
		}
		return provider, nil
	default:
		return nil, fmt.Errorf("unsupported delegation provider: %s", cfg.Provider)
	}
}
