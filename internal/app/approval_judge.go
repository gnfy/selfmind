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
// is bounded structured JSON, and the temperature is pinned to 0 for a
// deterministic verdict. This lives in the app layer (not internal/tools) so the
// triage logic stays model-agnostic and the concrete model choice is injected.
type llmApprovalJudge struct {
	provider  llm.Provider
	route     string
	timeout   time.Duration
	reasoning string
}

// judgeSystemPrompt reinforces the same structured contract as the per-call
// guardian prompt. Keeping both layers aligned prevents a system-level
// one-word instruction from silently discarding risk, authorization, and the
// rationale shown to the person.
const judgeSystemPrompt = `You decide whether this tool operation needs additional human confirmation by evaluating its actual effects, safety, and human authorization. A flag requesting review does not itself require human confirmation; your decision is the smart-mode operation review, subject to independently enforced safety boundaries. Reply with exactly one JSON object and no other text:
{"outcome":"approve|deny|escalate","risk_level":"low|medium|high|critical","user_authorization":"unknown|low|medium|high","rationale":"one short sentence"}
When uncertain, choose escalate.`

// judgeMaxTokens must cover both the compact JSON verdict and any hidden
// reasoning a provider still emits. A tight cap produces an HTTP 200 with no
// usable text, which safely escalates but silently turns smart mode into
// on-request: observed live on 2026-09-16, 51 of 58 human asks in one day were
// finish=length with 1024 reasoning tokens and zero verdict bytes. The parser
// still accepts only the bounded JSON contract, regardless of the provider's
// internal reasoning behavior.
const judgeMaxTokens = 4096

// judgeDefaultReasoning asks the provider not to reason. The verdict is a
// bounded classification, the same shape as post-run maintenance, which also
// runs at "none". Requesting "low" is not bounded on every provider: DeepSeek
// has no low tier, so the adapter maps it to the high tier with thinking
// enabled, and that reasoning consumed the whole output budget above. An
// explicit models.roles.<judge role>.reasoning still wins.
const judgeDefaultReasoning = "none"

// NewApprovalJudge builds a tools.ApprovalJudge backed by the given cheap role
// provider. Returns nil when the provider is nil so callers can wire it
// unconditionally: a nil judge makes smart mode degrade to a human ask, never an
// auto-approval.
func NewApprovalJudge(provider llm.Provider) tools.ApprovalJudge {
	if provider == nil {
		return nil
	}
	return &llmApprovalJudge{provider: provider, timeout: config.DefaultApprovalTriageTimeout, reasoning: judgeDefaultReasoning}
}

func NewConfiguredApprovalJudge(mem *memory.MemoryManager, cfg *config.Config, tenantID string) tools.ApprovalJudge {
	provider, role := configuredApprovalJudgeProvider(mem, cfg, tenantID)
	if provider == nil {
		log.Info("smart approval judge disabled: configure models.auxiliary or models.roles.fast_classifier to enable model triage without using the main model")
		return nil
	}
	if role != llm.RoleFastClassifier {
		log.Info("smart approval judge using legacy background_review role; configure models.auxiliary or models.roles.fast_classifier for lower latency")
	}
	return &llmApprovalJudge{
		provider:  provider,
		route:     string(role),
		timeout:   cfg.Agent.ApprovalTriageTimeoutDuration(),
		reasoning: approvalJudgeReasoning(cfg, role),
	}
}

// approvalJudgeReasoning honors an explicit reasoning setting on the judge's
// own role entry. The inherited models.auxiliary reasoning is not applied: it
// is tuned for the other background roles, and a thinking tier there would
// recreate the exhausted-budget failure the default exists to prevent.
func approvalJudgeReasoning(cfg *config.Config, role llm.ModelRole) string {
	if cfg != nil {
		if explicit, ok := cfg.Models.Roles[string(role)]; ok {
			if value := explicit.EffectiveReasoning(); value != "" {
				return value
			}
		}
	}
	return judgeDefaultReasoning
}

func (j *llmApprovalJudge) reasoningEffort() string {
	if j == nil || strings.TrimSpace(j.reasoning) == "" {
		return judgeDefaultReasoning
	}
	return j.reasoning
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
	if provider := configuredAuxiliaryRoleProvider(mem, cfg, tenantID, llm.RoleFastClassifier); provider != nil {
		return provider, llm.RoleFastClassifier
	}
	if provider := explicitRoleProvider(mem, cfg, tenantID, llm.RoleBackgroundReview); provider != nil {
		return provider, llm.RoleBackgroundReview
	}
	return nil, ""
}

func (j *llmApprovalJudge) Judge(ctx context.Context, prompt string) (string, error) {
	result, err := j.JudgeResponse(ctx, prompt)
	return result.Content, err
}

func (j *llmApprovalJudge) JudgeResponse(ctx context.Context, prompt string) (tools.ApprovalResponse, error) {
	resp, err := j.provider.Chat(ctx, llm.ChatRequest{
		SystemPrompt: judgeSystemPrompt,
		Messages:     []llm.Message{{Role: "user", Content: prompt}},
		MaxTokens:    judgeMaxTokens,
		// temperature 0 for a deterministic verdict; adapters that ignore the
		// option simply fall back to their default, which triage tolerates
		// (unrecognized replies escalate).
		Options: map[string]interface{}{"temperature": 0, "reasoning_effort": j.reasoningEffort(), "response_format": map[string]interface{}{"type": "json_object"}},
	})
	result := tools.ApprovalResponse{ApprovalResponseMetadata: tools.ApprovalResponseMetadata{Version: 1}}
	if err != nil {
		result.ProtocolStatus = "provider_error"
		return result, err
	}
	if resp != nil {
		result.Content = strings.TrimSpace(resp.Content)
		result.FinishReason = tools.RedactSensitive(resp.FinishReason)
		if len(result.FinishReason) > 80 {
			result.FinishReason = "unrecognized"
		}
		result.ResponseBytes = len(resp.Content)
		result.OutputTokens = resp.Usage.OutputTokens
		result.ReasoningTokens = resp.Usage.ReasoningOutputTokens
	}
	switch {
	case resp == nil || result.Content == "":
		result.ProtocolStatus = "empty_output"
	default:
		result.ProtocolStatus = "received"
	}
	if resp != nil {
		switch strings.ToLower(strings.TrimSpace(resp.FinishReason)) {
		case "length", "max_tokens", "max_output_tokens", "incomplete":
			result.ProtocolStatus = "output_limit"
		}
		if len(resp.ToolCalls) > 0 {
			result.ProtocolStatus = "unexpected_tool_call"
		}
	}
	if result.ProtocolStatus != "received" {
		return result, &tools.ApprovalResponseError{Class: result.ProtocolStatus, Metadata: result.ApprovalResponseMetadata}
	}
	return result, nil
}
