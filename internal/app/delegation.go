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

// MakeDelegateFn returns a delegate function configured from config.
func MakeDelegateFn(mem *memory.MemoryManager, backend kernel.AgentBackend, cfg config.DelegationConfig) func(goal, contextStr string, toolsets []string) (string, llm.UsageStats, error) {
	return func(goal, contextStr string, toolsets []string) (string, llm.UsageStats, error) {
		provider, err := makeDelegationProvider(cfg)
		if err != nil {
			return "", llm.UsageStats{}, err
		}

		// Create a sub-agent.
		// Note: We use the same backend (tools) but a fresh Agent instance.
		// We can also limit iterations for sub-agents.
		maxRetries := cfg.MaxRetries
		if maxRetries == 0 {
			maxRetries = 3
		}
		maxIter := cfg.MaxIterations
		if maxIter == 0 {
			maxIter = 50
		}

		// Filter tools based on toolsets
		var subBackend kernel.AgentBackend = backend
		if len(toolsets) > 0 {
			if disp, ok := backend.(*tools.Dispatcher); ok {
				subRegistry := tools.NewRegistry()
				allTools := disp.ListTools()

				// Map toolsets to specific tools
				requestedTools := make(map[string]bool)
				for _, ts := range toolsets {
					ts = strings.TrimSpace(ts)
					switch ts {
					case "file":
						requestedTools["read_file"] = true
						requestedTools["write_file"] = true
						requestedTools["ls_r"] = true
						requestedTools["search_files"] = true
						requestedTools["patch"] = true
					case "terminal", "shell":
						requestedTools["terminal"] = true
					case "web":
						requestedTools["web_search"] = true
						requestedTools["web_extract"] = true
					default:
						// Allow individual tool names too
						requestedTools[ts] = true
					}
				}

				for _, name := range allTools {
					if requestedTools[name] {
						if t, ok := disp.GetTool(name); ok {
							subRegistry.Register(t)
						}
					}
				}
				subBackend = tools.NewDispatcherWithRegistry(subRegistry)
			}
		}

		subAgent := kernel.NewAgent(mem, subBackend, provider, "You are a specialized sub-agent helping with a task.", maxIter, maxRetries, nil)

		fullPrompt := fmt.Sprintf("Target Goal: %s\nContext: %s\nAvailable Toolsets: %v\n\nPlease complete the task and return the final result.", goal, contextStr, toolsets)

		// Execute in a sub-context
		return subAgent.RunConversation(context.Background(), "system", "delegation", fullPrompt)
	}
}

func MakeDelegateBatchFn(mem *memory.MemoryManager, backend kernel.AgentBackend, cfg config.DelegationConfig) func(tasks []tools.DelegateTaskSpec) ([]tools.DelegateTaskResult, error) {
	return func(specs []tools.DelegateTaskSpec) ([]tools.DelegateTaskResult, error) {
		provider, err := makeDelegationProvider(cfg)
		if err != nil {
			return nil, err
		}
		maxIter := cfg.MaxIterations
		if maxIter == 0 {
			maxIter = 50
		}
		host := NewMultiAgentHost(backend, provider, mem, 5, 2, maxIter)
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
