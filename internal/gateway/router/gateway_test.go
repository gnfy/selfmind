package router

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
)

type intentLLMProvider struct {
	content string
}

func (p *intentLLMProvider) ChatCompletion(ctx context.Context, messages []llm.Message) (string, error) {
	return p.content, nil
}

func (p *intentLLMProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Content: p.content}, nil
}

func (p *intentLLMProvider) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent, 1)
	ch <- llm.StreamEvent{Content: p.content}
	close(ch)
	return ch, nil
}

func TestModelStatusReplyUsesConfiguredRuntimeLabel(t *testing.T) {
	gw := NewGateway(nil, nil)
	gw.SetModelDisplay("kimi-coding", "kimi-for-coding")

	resp := gw.ModelStatusReply()
	if !strings.Contains(resp, "kimi-coding/kimi-for-coding") {
		t.Fatalf("response = %q", resp)
	}
}

func TestIntentClassifierDefaultsOrdinaryLanguageToTask(t *testing.T) {
	classifier := NewIntentClassifier()
	tests := []string{
		"who are you?",
		"\u4f60\u662f\u8c01\uff1f",
		"write a Go binary search example",
		"\u53ef\u4ee5",
	}
	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			result := classifier.ClassifyDetailed(input)
			if result.Intent != IntentTask || !result.ShouldCreateTask || !result.ShouldUseTools {
				t.Fatalf("intent result = %+v", result)
			}
		})
	}
}

func TestIntentClassifierKeepsExplicitRoutes(t *testing.T) {
	classifier := NewIntentClassifier()
	tests := []struct {
		input string
		want  Intent
	}{
		{input: "/skill codebase-inspection", want: IntentSkill},
		{input: "/query last task", want: IntentQuery},
		{input: "/route cli", want: IntentRoute},
		{input: "continue", want: IntentContinue},
		{input: "\u7ee7\u7eed", want: IntentContinue},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := classifier.ClassifyDetailed(tt.input); got.Intent != tt.want {
				t.Fatalf("intent = %+v, want %v", got, tt.want)
			}
		})
	}
}

func TestIntentLLMDoesNotOverrideAgentFirstDefault(t *testing.T) {
	gw := NewGateway(nil, &intentLLMProvider{content: `{"intent":"casual","confidence":0.99,"reason":"chat"}`})
	gw.SetIntentClassifier(NewIntentClassifierWithRules(IntentRuleConfig{Mode: "llm"}))

	result := gw.ClassifyIntentWithContext(context.Background(), "maybe later", "cli")
	if result.Intent != IntentTask || result.Source != "rules" {
		t.Fatalf("intent result = %+v", result)
	}
}
