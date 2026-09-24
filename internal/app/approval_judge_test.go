package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	last     llm.ChatRequest
	reply    string
	response *llm.ChatResponse
}

func (p *judgeCaptureProvider) ChatCompletion(ctx context.Context, messages []llm.Message) (string, error) {
	return p.reply, nil
}

func (p *judgeCaptureProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	p.last = req
	if p.response != nil {
		return p.response, nil
	}
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
	// "none" is the only effort every adapter can bound: a provider without a
	// low tier maps "low" upward (DeepSeek: high with thinking enabled) and the
	// reasoning then consumed the whole cap (observed 2026-09-16: 51 of 58
	// human asks in one day were empty verdicts).
	if provider.last.Options["reasoning_effort"] != "none" {
		t.Fatalf("authorization review must not request provider reasoning by default: %#v", provider.last.Options)
	}
	format, ok := provider.last.Options["response_format"].(map[string]interface{})
	if !ok || format["type"] != "json_object" {
		t.Fatalf("approval judge must request structured JSON output: %#v", provider.last.Options)
	}
	if provider.last.MaxTokens < 2048 {
		t.Fatalf("MaxTokens = %d: too small to survive a provider that still reasons at its default tier", provider.last.MaxTokens)
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
	cfg.Auth.CredentialsFile = filepath.Join(t.TempDir(), "auth.json")
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

// TestConfiguredApprovalJudgeIgnoresConfiguredReasoning: every smart-mode tool
// call waits on the verdict inside the person's turn, so no configured level —
// inherited from the background route or set on the judge's own role — may
// slow it down.
func TestConfiguredApprovalJudgeIgnoresConfiguredReasoning(t *testing.T) {
	cfg := &config.Config{}
	cfg.Models.Primary = config.ModelSelectionConfig{Provider: "openai", Model: "primary-model"}
	cfg.Models.Auxiliary = config.ModelSelectionConfig{Provider: "openai", Model: "aux-model", Reasoning: "high"}
	cfg.Providers.OpenAI.APIKey = "test-key"
	cfg.Normalize()
	judge, ok := NewConfiguredApprovalJudge(nil, cfg, "default").(*llmApprovalJudge)
	if !ok || judge == nil {
		t.Fatalf("configured judge = %T", judge)
	}
	if got := judge.reasoningEffort(); got != "none" {
		t.Fatalf("auxiliary reasoning leaked into the judge: %q", got)
	}
	for _, level := range []string{"low", "high", "xhigh"} {
		cfg.Models.Roles = map[string]config.ModelRoleConfig{
			string(llm.RoleFastClassifier): {Provider: "openai", Model: "fast-model", APIKey: "test-key", Reasoning: level},
		}
		cfg.Normalize()
		judge, ok = NewConfiguredApprovalJudge(nil, cfg, "default").(*llmApprovalJudge)
		if !ok || judge == nil {
			t.Fatalf("configured judge = %T", judge)
		}
		if got := judge.reasoningEffort(); got != "none" {
			t.Fatalf("explicit fast_classifier reasoning %q reached the judge: %q", level, got)
		}
	}
}

func TestConfiguredApprovalJudgeUsesProviderCapabilityFloor(t *testing.T) {
	cfg := &config.Config{}
	cfg.Models.Primary = config.ModelSelectionConfig{Provider: "google", Model: "gemini-3.8-flash"}
	cfg.Models.Auxiliary = config.ModelSelectionConfig{Provider: "google", Model: "gemini-3.8-flash"}
	cfg.Providers.Google.APIKey = "test-key"
	cfg.Normalize()
	judge, ok := NewConfiguredApprovalJudge(nil, cfg, "default").(*llmApprovalJudge)
	if !ok || judge == nil {
		t.Fatalf("configured judge = %T", judge)
	}
	if got := judge.reasoningEffort(); got != "low" {
		t.Fatalf("gemini 3 approval reasoning = %q, want lowest supported tier", got)
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

func TestApprovalJudgeRejectsIncompleteDecision(t *testing.T) {
	for _, reason := range []string{"length", "max_tokens"} {
		t.Run(reason, func(t *testing.T) {
			provider := &judgeCaptureProvider{response: &llm.ChatResponse{
				Content:      `{"outcome":"approve","risk_level":"low","user_authorization":"high","rationale":"Allowed"}`,
				FinishReason: reason, Usage: llm.UsageStats{OutputTokens: 1024, ReasoningOutputTokens: 1000},
			}}
			_, err := NewApprovalJudge(provider).Judge(context.Background(), "review")
			if err == nil || !strings.Contains(err.Error(), "output_limit") {
				t.Fatalf("incomplete decision must not authorize execution: %v", err)
			}
		})
	}
}

func TestApprovalJudgePreservesFullUsage(t *testing.T) {
	p := &judgeCaptureProvider{response: &llm.ChatResponse{Content: `{"outcome":"escalate"}`, Usage: llm.UsageStats{InputTokens: 100, OutputTokens: 20, CacheReadInputTokens: 70, CacheMissInputTokens: 30, ReasoningOutputTokens: 5, CacheUsageReported: true}}}
	j := NewApprovalJudge(p).(tools.StructuredApprovalJudge)
	r, err := j.JudgeResponse(context.Background(), "review")
	if err != nil || r.Usage == nil || r.Usage.InputTokens != 100 || r.Usage.CacheReadInputTokens != 70 || !r.Usage.CacheUsageReported || r.OutputTokens != 20 {
		t.Fatalf("usage lost: %+v %v", r, err)
	}
}

// The judge asks for no reasoning, and that intent must reach the wire. It did
// not: judge.reasoningEffort() returned "none" all along — the assertion the
// tests above make — while the adapter dropped the value, so a model that
// reasons by default spent ~400 reasoning tokens per verdict and timed out
// behind the 10s triage budget. Only the request body shows that, so this
// drives the real judge from a YAML declaration to the bytes it sends.
func TestApprovalJudgeDisablesReasoningOnTheWireWhenTheProviderDeclaresHow(t *testing.T) {
	judgeBody := func(t *testing.T, quirks string) map[string]interface{} {
		t.Helper()
		var got map[string]interface{}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			w.Header().Set("content-type", "application/json")
			fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"outcome\":\"approve\",\"risk_level\":\"low\",\"user_authorization\":\"high\",\"rationale\":\"ok\"}"},"finish_reason":"stop"}]}`)
		}))
		defer server.Close()

		// Mirrors a real custom OpenAI-compatible provider serving both Main
		// and Background, with the judge inheriting Background.
		path := filepath.Join(t.TempDir(), "config.yaml")
		yaml := fmt.Sprintf(`
providers:
  custom:
    bailian:
      base_url: %q
      protocol: openai-compatible
      auth: bearer
      api_key: test-key
%s
models:
  primary:
    provider: bailian
    model: qwen3.8-flash
    reasoning: high
  auxiliary:
    follow_primary: true
    provider: bailian
    model: qwen3.8-flash
`, server.URL, quirks)
		if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.LoadConfig(config.Options{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		judge, ok := NewConfiguredApprovalJudge(nil, cfg, "default").(*llmApprovalJudge)
		if !ok || judge == nil {
			t.Fatalf("configured judge = %T", judge)
		}
		if _, err := judge.JudgeResponse(context.Background(), "verdict please"); err != nil {
			t.Fatal(err)
		}
		return got
	}

	declared := judgeBody(t, "      quirks:\n        thinking_mode: effort_none")
	if declared["reasoning_effort"] != "none" {
		t.Fatalf("declared effort_none: the judge must send reasoning_effort \"none\", got %#v", declared["reasoning_effort"])
	}

	// The constraint that must change the result: without the declaration the
	// provider keeps the conservative omission — the exact request that reached
	// production and let the model reason by default.
	undeclared := judgeBody(t, "")
	if _, present := undeclared["reasoning_effort"]; present {
		t.Fatalf("an undeclared provider must keep omitting the parameter, got %#v", undeclared["reasoning_effort"])
	}
}
