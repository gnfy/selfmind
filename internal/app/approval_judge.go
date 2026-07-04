package app

import (
	"context"
	"strings"

	"selfmind/internal/kernel/llm"
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

// judgeMaxTokens caps the reply. A verdict is one word; a tiny cap keeps the
// call cheap and blocks a chatty model from burning tokens.
const judgeMaxTokens = 8

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
