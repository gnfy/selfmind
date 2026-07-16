package app

import (
	"context"
	"fmt"
	"strings"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/platform/log"
)

// namedMaintenanceProvider is an explicitly configured cheap/background
// provider. The chain never invents a fallback to the primary coding model;
// every role in it must be listed under models.roles.
type namedMaintenanceProvider struct {
	role     llm.ModelRole
	provider llm.Provider
}

type maintenanceProviderChain struct {
	providers []namedMaintenanceProvider
}

func (c *maintenanceProviderChain) ChatCompletion(ctx context.Context, messages []llm.Message) (string, error) {
	var failures []string
	for _, candidate := range c.providers {
		content, err := candidate.provider.ChatCompletion(ctx, messages)
		if err == nil {
			return content, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		failures = append(failures, fmt.Sprintf("%s: %v", candidate.role, err))
		log.Warn("maintenance provider unavailable; trying explicit fallback", "role", candidate.role, "error", err)
	}
	return "", fmt.Errorf("maintenance providers unavailable: %s", strings.Join(failures, "; "))
}

func (c *maintenanceProviderChain) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	var failures []string
	for _, candidate := range c.providers {
		resp, err := candidate.provider.Chat(ctx, req)
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		failures = append(failures, fmt.Sprintf("%s: %v", candidate.role, err))
		log.Warn("maintenance provider unavailable; trying explicit fallback", "role", candidate.role, "error", err)
	}
	return nil, fmt.Errorf("maintenance providers unavailable: %s", strings.Join(failures, "; "))
}

func (c *maintenanceProviderChain) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	var failures []string
	for _, candidate := range c.providers {
		stream, err := candidate.provider.StreamChat(ctx, req)
		if err == nil {
			return stream, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		failures = append(failures, fmt.Sprintf("%s: %v", candidate.role, err))
		log.Warn("maintenance provider unavailable; trying explicit fallback", "role", candidate.role, "error", err)
	}
	return nil, fmt.Errorf("maintenance providers unavailable: %s", strings.Join(failures, "; "))
}
