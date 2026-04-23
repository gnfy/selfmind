package memory

import (
	"context"
	"log/slog"
	"strings"
)

// MessagePair represents a single turn of conversation for memory sync.
type MessagePair struct {
	User      string
	Assistant string
}

// MemoryProvider is the interface for external memory backends (e.g., Honcho, Hindsight, Mem0).
// Implementations can be registered with MemoryManager to enable pluggable long-term memory.
type MemoryProvider interface {
	// Name returns the provider identifier.
	Name() string

	// Prefetch recalls relevant context before a turn starts.
	// Called synchronously; should return quickly (use background caching if needed).
	Prefetch(ctx context.Context, tenantID, query string) (string, error)

	// SyncTurn persists a completed turn to the backend.
	// Called after each assistant response (including tool-calling turns).
	// Should be non-blocking — queue for background processing if the backend has latency.
	SyncTurn(ctx context.Context, tenantID string, messages []MessagePair) error

	// SyncMessages persists the full message list of the current turn.
	// Used for providers that need the complete conversation context.
	SyncMessages(ctx context.Context, tenantID string, messagesJSON []byte) error

	// Shutdown flushes queues and closes connections.
	Shutdown() error
}

// RegisterProvider registers an external memory provider.
// MemoryManager supports multiple providers; each contributes to prefetch and sync.
func (m *MemoryManager) RegisterProvider(p MemoryProvider) {
	if m.providers == nil {
		m.providers = make(map[string]MemoryProvider)
	}
	m.providers[p.Name()] = p
}

// PrefetchAll collects recalled context from all registered providers.
func (m *MemoryManager) PrefetchAll(ctx context.Context, tenantID, query string) string {
	var parts []string
	for _, p := range m.providers {
		result, err := p.Prefetch(ctx, tenantID, query)
		if err != nil {
			slog.Debug("memory provider prefetch failed", "provider", p.Name(), "error", err)
			continue
		}
		if result != "" {
			parts = append(parts, result)
		}
	}
	return joinParts(parts)
}

// SyncTurnAll syncs a completed turn to all registered providers.
func (m *MemoryManager) SyncTurnAll(ctx context.Context, tenantID string, pairs []MessagePair) {
	for _, p := range m.providers {
		if err := p.SyncTurn(ctx, tenantID, pairs); err != nil {
			slog.Warn("memory provider sync failed", "provider", p.Name(), "error", err)
		}
	}
}

// SyncMessagesAll syncs the full message list to all registered providers.
func (m *MemoryManager) SyncMessagesAll(ctx context.Context, tenantID string, messagesJSON []byte) {
	for _, p := range m.providers {
		if err := p.SyncMessages(ctx, tenantID, messagesJSON); err != nil {
			slog.Warn("memory provider sync messages failed", "provider", p.Name(), "error", err)
		}
	}
}

// ShutdownProviders shuts down all external memory providers.
func (m *MemoryManager) ShutdownProviders() {
	for _, p := range m.providers {
		if err := p.Shutdown(); err != nil {
			slog.Warn("memory provider shutdown failed", "provider", p.Name(), "error", err)
		}
	}
}

func joinParts(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, p := range parts {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(p)
	}
	return sb.String()
}
