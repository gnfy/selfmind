package app

import (
	"context"
	"testing"

	"selfmind/internal/kernel/llm"
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
// every cheap role model in use is a REASONING model, so an output cap sized for
// the one-word verdict alone (the old 8) was consumed by thinking. The judge then
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
	if provider.last.MaxTokens < 256 {
		t.Fatalf("MaxTokens = %d: too small for a reasoning model's thinking plus verdict", provider.last.MaxTokens)
	}
	if provider.last.SystemPrompt == "" {
		t.Fatal("the one-word contract must be reinforced at the system level")
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
