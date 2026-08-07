package app

import (
	"context"
	"strings"
	"time"

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
	route    string
	timeout  time.Duration
}

// judgeSystemPrompt reinforces the same structured contract as the per-call
// guardian prompt. Keeping both layers aligned prevents a system-level
// one-word instruction from silently discarding risk, authorization, and the
// rationale shown to the person.
const judgeSystemPrompt = `You are a command-safety triage judge. Reply with exactly one JSON object and no other text:
{"risk_level":"low|medium|high|critical","user_authorization":"unknown|low|medium|high","outcome":"approve|deny|escalate","rationale":"one short sentence"}
When uncertain, choose escalate.`

// judgeMaxTokens must cover both the compact JSON verdict and any hidden
// reasoning emitted by cheap reasoning models. A tiny cap can produce an HTTP
// 200 response with no usable text, which safely escalates but silently turns
// smart mode into on-request. The parser still accepts only the bounded JSON
// contract, regardless of the provider's internal reasoning behavior.
const judgeMaxTokens = 1024

// NewApprovalJudge builds a tools.ApprovalJudge backed by the given cheap role
// provider. Returns nil when the provider is nil so callers can wire it
// unconditionally: a nil judge makes smart mode degrade to a human ask, never an
// auto-approval.
func NewApprovalJudge(provider llm.Provider) tools.ApprovalJudge {
	if provider == nil {
		return nil
	}
	return &llmApprovalJudge{provider: provider, timeout: config.DefaultApprovalTriageTimeout}
}

func NewConfiguredApprovalJudge(mem *memory.MemoryManager, cfg *config.Config, tenantID string) tools.ApprovalJudge {
	provider, role := configuredApprovalJudgeProvider(mem, cfg, tenantID)
	if provider == nil {
		log.Info("smart approval judge disabled: configure models.roles.fast_classifier to enable model triage without using the main model")
		return nil
	}
	if role != llm.RoleFastClassifier {
		log.Info("smart approval judge using legacy background_review role; configure models.roles.fast_classifier for lower latency")
	}
	return &llmApprovalJudge{
		provider: provider,
		route:    string(role),
		timeout:  cfg.Agent.ApprovalTriageTimeoutDuration(),
	}
}

func (j *llmApprovalJudge) ApprovalJudgeRoute() string { return j.route }

func (j *llmApprovalJudge) ApprovalJudgeTimeout() time.Duration {
	if j == nil || j.timeout <= 0 {
		return config.DefaultApprovalTriageTimeout
	}
	return j.timeout
}

// configuredApprovalJudgeProvider keeps approval latency independent from
// background review. Older configs that only declared background_review remain
// functional, but the foreground coding provider is never borrowed silently.
func configuredApprovalJudgeProvider(mem *memory.MemoryManager, cfg *config.Config, tenantID string) (llm.Provider, llm.ModelRole) {
	if provider := explicitRoleProvider(mem, cfg, tenantID, llm.RoleFastClassifier); provider != nil {
		return provider, llm.RoleFastClassifier
	}
	if provider := explicitRoleProvider(mem, cfg, tenantID, llm.RoleBackgroundReview); provider != nil {
		return provider, llm.RoleBackgroundReview
	}
	return nil, ""
}

func (j *llmApprovalJudge) Judge(ctx context.Context, prompt string) (string, error) {
	resp, err := j.provider.Chat(ctx, llm.ChatRequest{
		SystemPrompt: judgeSystemPrompt,
		Messages:     []llm.Message{{Role: "user", Content: prompt}},
		MaxTokens:    judgeMaxTokens,
		// temperature 0 for a deterministic verdict; adapters that ignore the
		// option simply fall back to their default, which triage tolerates
		// (unrecognized replies escalate).
		Options: map[string]interface{}{"temperature": 0, "reasoning_effort": "low"},
	})
	if err != nil {
		return "", err
	}
	if resp == nil {
		return "", nil
	}
	return strings.TrimSpace(resp.Content), nil
}
