package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/platform/config"
	"selfmind/internal/tools"
)

// judgeCaptureProvider records the request the judge sends so tests can pin the
// output budget and determinism settings.
type judgeCaptureProvider struct {
	last  llm.ChatRequest
	reply string
}

func (p *judgeCaptureProvider) ChatCompletion(ctx context.Context, messages []llm.Message) (string, error) {
	return p.reply, nil
}

func (p *judgeCaptureProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	p.last = req
	return &llm.ChatResponse{Content: p.reply}, nil
}

func (p *judgeCaptureProvider) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent)
	close(ch)
	return ch, nil
}

// TestApprovalJudgeBudgetCoversReasoning pins the fix for a silent failure mode:
// every cheap role model in use may be a REASONING model, so an output cap sized
// for the verdict alone (the old 8) was consumed by thinking. The judge then
// returned nothing parseable, triage escalated every time, and smart mode became
// on-request while still looking strict. The budget must leave room for reasoning
// plus the verdict.
func TestApprovalJudgeBudgetCoversReasoning(t *testing.T) {
	provider := &judgeCaptureProvider{reply: "APPROVE"}
	judge := NewApprovalJudge(provider)
	if judge == nil {
		t.Fatal("judge should be built for a non-nil provider")
	}
	if _, err := judge.Judge(context.Background(), "Tool: terminal\nCommand: git status"); err != nil {
		t.Fatalf("judge: %v", err)
	}
	if provider.last.MaxTokens < 512 {
		t.Fatalf("MaxTokens = %d: too small for a reasoning model's thinking plus verdict", provider.last.MaxTokens)
	}
	if !strings.Contains(provider.last.SystemPrompt, `"risk_level"`) || !strings.Contains(provider.last.SystemPrompt, `"rationale"`) {
		t.Fatal("the structured guardian contract must be reinforced at the system level")
	}
}

func TestConfiguredApprovalJudgeProviderPrefersFastClassifier(t *testing.T) {
	cfg := &config.Config{}
	cfg.Models.Primary = config.ModelSelectionConfig{Provider: "openai", Model: "primary-model"}
	cfg.Models.Roles = map[string]config.ModelRoleConfig{
		string(llm.RoleFastClassifier):   {Provider: "openai", Model: "fast-model", APIKey: "test-key"},
		string(llm.RoleBackgroundReview): {Provider: "openai", Model: "review-model", APIKey: "test-key"},
	}
	cfg.Normalize()
	provider, role := configuredApprovalJudgeProvider(nil, cfg, "default")
	if provider == nil || role != llm.RoleFastClassifier {
		t.Fatalf("provider=%T role=%q, want explicit fast_classifier", provider, role)
	}
	if got := llm.GetModelName(provider); got != "fast-model" {
		t.Fatalf("model=%q, want fast-model", got)
	}
}

func TestConfiguredApprovalJudgeProviderUsesAuxiliaryModel(t *testing.T) {
	cfg := &config.Config{}
	cfg.Models.Primary = config.ModelSelectionConfig{Provider: "openai", Model: "primary-model"}
	cfg.Models.Auxiliary = config.ModelSelectionConfig{Provider: "openai", Model: "aux-model"}
	cfg.Providers.OpenAI.APIKey = "test-key"
	cfg.Normalize()
	provider, role := configuredApprovalJudgeProvider(nil, cfg, "default")
	if provider == nil || role != llm.RoleFastClassifier {
		t.Fatalf("provider=%T role=%q, want auxiliary fast_classifier", provider, role)
	}
	if got := llm.GetModelName(provider); got != "aux-model" {
		t.Fatalf("model=%q, want aux-model", got)
	}
}

func TestConfiguredApprovalJudgeProviderLegacyFallbackOnly(t *testing.T) {
	cfg := &config.Config{}
	cfg.Models.Primary = config.ModelSelectionConfig{Provider: "openai", Model: "primary-model"}
	cfg.Models.Roles = map[string]config.ModelRoleConfig{
		string(llm.RoleBackgroundReview): {Provider: "openai", Model: "review-model", APIKey: "test-key"},
	}
	cfg.Normalize()
	provider, role := configuredApprovalJudgeProvider(nil, cfg, "default")
	if provider == nil || role != llm.RoleBackgroundReview {
		t.Fatalf("provider=%T role=%q, want legacy background_review", provider, role)
	}

	cfg.Models.Roles = nil
	if provider, role := configuredApprovalJudgeProvider(nil, cfg, "default"); provider != nil || role != "" {
		t.Fatalf("primary-only config must not feed approval triage: provider=%T role=%q", provider, role)
	}
}

func TestConfiguredApprovalJudgeUsesConfiguredTimeout(t *testing.T) {
	cfg := &config.Config{}
	cfg.Agent.ApprovalTriageTimeout = "45s"
	cfg.Models.Roles = map[string]config.ModelRoleConfig{
		string(llm.RoleFastClassifier): {Provider: "openai", Model: "fast-model", APIKey: "test-key"},
	}
	cfg.Normalize()
	judge := NewConfiguredApprovalJudge(nil, cfg, "default")
	timed, ok := judge.(tools.ApprovalJudgeTimeout)
	if !ok {
		t.Fatalf("configured judge does not expose its foreground timeout: %T", judge)
	}
	if got := timed.ApprovalJudgeTimeout(); got != 45*time.Second {
		t.Fatalf("ApprovalJudgeTimeout = %v, want 45s", got)
	}
}

// TestApprovalJudgeNilProviderStaysNil keeps the fail-safe wiring: no judge must
// mean "ask the human", never "auto-approve".
func TestApprovalJudgeNilProviderStaysNil(t *testing.T) {
	if judge := NewApprovalJudge(nil); judge != nil {
		t.Fatal("a nil provider must not produce a judge that could auto-approve")
	}
	var judge tools.ApprovalJudge = NewApprovalJudge(nil)
	if judge != nil {
		t.Fatal("nil judge must satisfy the tools contract as nil")
	}
}
