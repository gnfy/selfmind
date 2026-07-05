package router

import (
	"context"
	"testing"
	"time"

	"selfmind/internal/kernel/llm"
)

// countingIntentProvider records how many Chat calls it received so a test can
// assert the LLM was (or was not) consulted, and returns a fixed body.
type countingIntentProvider struct {
	content string
	calls   int
}

func (p *countingIntentProvider) ChatCompletion(ctx context.Context, messages []llm.Message) (string, error) {
	return p.content, nil
}

func (p *countingIntentProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	p.calls++
	return &llm.ChatResponse{Content: p.content}, nil
}

func (p *countingIntentProvider) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent, 1)
	ch <- llm.StreamEvent{Content: p.content}
	close(ch)
	return ch, nil
}

func recentTask() RecentTaskContext {
	return RecentTaskContext{Title: "build a KOF game", Summary: "generated index.html", Status: "in_progress", Age: 2 * time.Minute}
}

// TestUpgradeTaskToContinueWhenLLMSaysContinue: a "continue" verdict upgrades the
// rules IntentTask to IntentContinue, sourced from the llm.
func TestUpgradeTaskToContinueWhenLLMSaysContinue(t *testing.T) {
	provider := &countingIntentProvider{content: `{"decision":"continue","confidence":0.9,"reason":"reacting to the game result"}`}
	gw := NewGateway(nil, nil, nil, provider)
	gw.SetIntentClassifier(NewIntentClassifierWithRules(IntentRuleConfig{Mode: "hybrid"}))

	result, ok := gw.UpgradeTaskToContinueWithLLM(context.Background(), "质量太差了", "weixin", recentTask())
	if !ok {
		t.Fatalf("expected upgrade, got ok=false result=%+v", result)
	}
	if result.Intent != IntentContinue || result.Source != "llm" || !result.ShouldCreateTask {
		t.Fatalf("upgraded result = %+v", result)
	}
	if provider.calls != 1 {
		t.Fatalf("expected exactly one llm call, got %d", provider.calls)
	}
}

// TestUpgradeKeepsTaskWhenLLMSaysNew: a "new" verdict must not upgrade — the
// rules IntentTask stands (never downgraded either).
func TestUpgradeKeepsTaskWhenLLMSaysNew(t *testing.T) {
	provider := &countingIntentProvider{content: `{"decision":"new","confidence":0.8,"reason":"unrelated request"}`}
	gw := NewGateway(nil, nil, nil, provider)
	gw.SetIntentClassifier(NewIntentClassifierWithRules(IntentRuleConfig{Mode: "hybrid"}))

	if _, ok := gw.UpgradeTaskToContinueWithLLM(context.Background(), "write a python script", "cli", recentTask()); ok {
		t.Fatal("a 'new' verdict must not upgrade to continue")
	}
}

// TestUpgradeKeepsTaskOnGarbage: an unparsable body never upgrades (fail safe).
func TestUpgradeKeepsTaskOnGarbage(t *testing.T) {
	provider := &countingIntentProvider{content: "not json at all"}
	gw := NewGateway(nil, nil, nil, provider)
	gw.SetIntentClassifier(NewIntentClassifierWithRules(IntentRuleConfig{Mode: "hybrid"}))

	if _, ok := gw.UpgradeTaskToContinueWithLLM(context.Background(), "anything", "cli", recentTask()); ok {
		t.Fatal("garbage llm output must not upgrade")
	}
}

// TestUpgradeNeverConsultsInRulesMode: rules mode must never call the LLM.
func TestUpgradeNeverConsultsInRulesMode(t *testing.T) {
	provider := &countingIntentProvider{content: `{"decision":"continue","confidence":0.9}`}
	gw := NewGateway(nil, nil, nil, provider)
	gw.SetIntentClassifier(NewIntentClassifierWithRules(IntentRuleConfig{Mode: "rules"}))

	if _, ok := gw.UpgradeTaskToContinueWithLLM(context.Background(), "anything", "cli", recentTask()); ok {
		t.Fatal("rules mode must not upgrade")
	}
	if provider.calls != 0 {
		t.Fatalf("rules mode must not call the llm, got %d calls", provider.calls)
	}
}
