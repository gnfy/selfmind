package app

import (
	"context"
	"strings"

	"selfmind/internal/gateway/httpapi"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/config"
	"selfmind/internal/platform/log"
)

// llmRunLabeler implements httpapi.RunLabeler over a cheap role-routed
// provider — the post-run labeling judge of Work Timeline P3. Mirrors the
// ApprovalJudge wiring: the provider is the memory_extract role (kept OFF the
// run's main coding provider), the reply is bounded to one short line, and
// temperature is pinned to 0 for a deterministic verdict. Lives in the app
// layer so the gateway's labeling logic stays model-agnostic and the concrete
// model choice is injected.
type llmRunLabeler struct {
	provider llm.Provider
}

// labelerSystemPrompt reinforces the one-line contract at the system level;
// the per-call prompt (httpapi.buildRunLabelPrompt) carries the labels, the
// turn summary, and the untrusted-data delimiters.
const labelerSystemPrompt = "You are a work-run labeling judge. Reply with exactly one line: KEEP, MOVE:<task_id>, or TITLE:<short title>. No explanation."

// labelerMaxTokens caps the reply: an id or a short title, never prose.
const labelerMaxTokens = 60

// NewRunLabeler builds an httpapi.RunLabeler backed by the given cheap role
// provider. Returns nil when the provider is nil so callers can wire it
// unconditionally: a nil labeler means every pre-label guess is simply kept
// (labels never gate context, so nothing else degrades).
func NewRunLabeler(provider llm.Provider) httpapi.RunLabeler {
	if provider == nil {
		return nil
	}
	return &llmRunLabeler{provider: provider}
}

func NewConfiguredRunLabeler(mem *memory.MemoryManager, cfg *config.Config, tenantID string) httpapi.RunLabeler {
	provider := explicitRoleProvider(mem, cfg, tenantID, llm.RoleMemoryExtract)
	if provider == nil {
		log.Info("run labeler disabled: configure models.roles.memory_extract to enable post-run relabeling without using the main model")
		return nil
	}
	return NewRunLabeler(provider)
}

func (l *llmRunLabeler) Label(ctx context.Context, prompt string) (string, error) {
	resp, err := l.provider.Chat(ctx, llm.ChatRequest{
		SystemPrompt: labelerSystemPrompt,
		Messages:     []llm.Message{{Role: "user", Content: prompt}},
		MaxTokens:    labelerMaxTokens,
		// temperature 0 for a deterministic verdict; adapters that ignore the
		// option fall back to their default, which the parser tolerates
		// (unrecognized replies degrade to KEEP).
		Options: map[string]interface{}{"temperature": 0},
	})
	if err != nil {
		return "", err
	}
	if resp == nil {
		return "", nil
	}
	return strings.TrimSpace(resp.Content), nil
}
