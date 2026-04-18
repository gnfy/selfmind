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
		if cfg.APIKey == "" {
			return "", llm.UsageStats{}, fmt.Errorf("delegation API key not configured")
		}

		// Create a specific provider for delegation if configured, otherwise use default
		var provider llm.Provider
		switch cfg.Provider {
		case "anthropic":
			provider = llm.NewAnthropicAdapter(cfg.APIKey)
			if cfg.Model != "" {
				if a, ok := provider.(*llm.AnthropicAdapter); ok {
					a.Model = cfg.Model
				}
			}
		case "openai":
			provider = llm.NewOpenAIAdapter(cfg.APIKey)
			if cfg.Model != "" {
				if o, ok := provider.(*llm.OpenAIAdapter); ok {
					o.Model = cfg.Model
				}
			}
		default:
			return "", llm.UsageStats{}, fmt.Errorf("unsupported delegation provider: %s", cfg.Provider)
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
