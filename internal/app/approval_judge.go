package app

import (
	"context"
	"strings"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/config"
	"selfmind/internal/platform/log"
	"selfmind/internal/tools"
)

// llmApprovalJudge implements tools.ApprovalJudge over a cheap role-routed
// provider. It is the concrete judge the smart-mode triage step (H2) calls: the
// provider is a role model (kept OFF the run's main coding provider), the reply
// is bounded to a single word, and the temperature is pinned to 0 for a
// deterministic verdict. This lives in the app layer (not internal/tools) so the
// triage logic stays model-agnostic and the concrete model choice is injected.
type llmApprovalJudge struct {
	provider llm.Provider
}

// judgeSystemPrompt reinforces the one-word contract at the system level; the
// per-call prompt (tools.buildTriagePrompt) carries the command and the
// prompt-injection defense.
const judgeSystemPrompt = "You are a command-safety triage judge. Reply with exactly one word: APPROVE, DENY, or ESCALATE. No explanation."

// judgeMaxTokens caps the reply. The verdict itself is one word, but the cap
// must also cover any REASONING the model emits before it — and every cheap role
// model in practice is a reasoning model. At the previous cap of 8 the thinking
// consumed the whole budget, so anthropic-protocol judges returned a response
// with no text block at all ("HTTP 200 response contained no text or tool use",
// stop_reason=max_tokens) and OpenAI-protocol judges returned truncated prose
// that no strict verdict parse could accept. Both outcomes escalate, which
// silently turned smart mode into on-request: the funnel looked strict when it
// was simply never ruling.
//
// 512 is measured, not guessed: it is the smallest budget at which the three
// cheap routes in use (nvidia/nemotron via OpenAI chat, kimi-for-coding and
// MiniMax via anthropic messages) all emit a bare verdict, and all answer in
// 0.4-5.4s — comfortably inside tools' 15s triage bound. Thinking blocks are
// dropped by the adapter, so the judge still receives exactly one word and the
// strict parser stays strict.
const judgeMaxTokens = 512

// NewApprovalJudge builds a tools.ApprovalJudge backed by the given cheap role
// provider. Returns nil when the provider is nil so callers can wire it
// unconditionally: a nil judge makes smart mode degrade to a human ask, never an
// auto-approval.
func NewApprovalJudge(provider llm.Provider) tools.ApprovalJudge {
	if provider == nil {
		return nil
	}
	return &llmApprovalJudge{provider: provider}
}

func NewConfiguredApprovalJudge(mem *memory.MemoryManager, cfg *config.Config, tenantID string) tools.ApprovalJudge {
	provider := explicitRoleProvider(mem, cfg, tenantID, llm.RoleBackgroundReview)
	if provider == nil {
		log.Info("smart approval judge disabled: configure models.roles.background_review to enable model triage without using the main model")
		return nil
	}
	return NewApprovalJudge(provider)
}

func (j *llmApprovalJudge) Judge(ctx context.Context, prompt string) (string, error) {
	resp, err := j.provider.Chat(ctx, llm.ChatRequest{
		SystemPrompt: judgeSystemPrompt,
		Messages:     []llm.Message{{Role: "user", Content: prompt}},
		MaxTokens:    judgeMaxTokens,
		// temperature 0 for a deterministic verdict; adapters that ignore the
		// option simply fall back to their default, which triage tolerates
		// (unrecognized replies escalate).
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
