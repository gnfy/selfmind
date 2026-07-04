package tools

// Deterministic integration coverage for the background-review auto-create
// mechanism (kernel.BackgroundReviewEngine.SpawnReview). This test lives in
// internal/tools rather than internal/kernel because the review agent needs a
// REAL skill_manage-capable backend to persist a skill, and internal/tools
// already imports internal/kernel (skill_manage.go), so hosting the test in
// kernel would create an import cycle. tools.Dispatcher implements
// kernel.AgentBackend directly, so the real code path is exercised end to end:
// SpawnReview goroutine -> restricted review backend -> NewAgent ->
// native tool call -> Dispatcher -> SkillManageTool -> disk + learning audit.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

// scriptedReviewProvider drives the review agent deterministically and offline:
// the first StreamChat returns a native skill_manage(create) tool call, the
// second returns the final plain-text summary. This mirrors how the kernel
// agent actually consumes providers (StreamChat is the primary path; Chat is
// only the non-streaming fallback).
type scriptedReviewProvider struct {
	requests []llm.ChatRequest
}

func (p *scriptedReviewProvider) ChatCompletion(ctx context.Context, messages []llm.Message) (string, error) {
	return "skill created: review-flow", nil
}

func (p *scriptedReviewProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Content: "skill created: review-flow"}, nil
}

func (p *scriptedReviewProvider) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.requests = append(p.requests, req)
	ch := make(chan llm.StreamEvent, 1)
	if len(p.requests) == 1 {
		ch <- llm.StreamEvent{ToolCalls: []llm.ToolCall{{
			ID:       "call-review-1",
			Function: "skill_manage",
			Args:     `{"action":"create","name":"review-flow","description":"Reusable code review workflow","content":"Steps:\n- Inspect the diff\n- Run focused tests\n- Summarize findings\n","source":"agent-created"}`,
		}}}
	} else {
		ch <- llm.StreamEvent{Content: "skill created: review-flow"}
	}
	close(ch)
	return ch, nil
}

func TestSpawnReviewCreatesSkillThroughRealToolchain(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	storage, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatalf("sqlite provider: %v", err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	mem := memory.NewMemoryManager(storage)

	// Real backend: the same Dispatcher/Registry stack production uses, with
	// the real skill_manage tool plus one deliberately non-allowlisted tool so
	// we can prove the restricted review backend filters tool exposure.
	registry := NewRegistry()
	disp := NewDispatcherWithRegistry(registry)
	disp.RegisterTool(NewSkillManageTool())
	disp.RegisterTool(&BaseTool{
		name:        "terminal",
		description: "must never be exposed to the background review agent",
		schema:      ToolSchema{Type: "object"},
		handler: func(args map[string]interface{}) (string, error) {
			t.Error("background review dispatched a non-allowlisted tool")
			return "", nil
		},
	})

	provider := &scriptedReviewProvider{}
	engine := kernel.NewBackgroundReviewEngine(mem, disp, provider, kernel.EvolutionConfig{Enabled: true}, 4, 1)
	notify := make(chan string, 1)
	engine.SetNotifyChannel(notify)

	engine.SpawnReview("default", "cli", []llm.Message{
		{Role: "user", Content: "please review my Go change and run the tests"},
		{Role: "assistant", Content: "Reviewed the diff, ran focused tests, summarized findings."},
	}, false, true)

	// SpawnReview runs a goroutine; the notify channel is its completion
	// signal (bounded wait, no unconditional sleeps).
	var note string
	select {
	case note = <-notify:
	case <-time.After(30 * time.Second):
		t.Fatal("background review did not complete within the deadline")
	}
	if !strings.HasPrefix(note, "review:") || !strings.Contains(note, "skill created: review-flow") {
		t.Fatalf("unexpected review notification: %q", note)
	}

	// The restricted backend must expose skill_manage (allowlisted) and must
	// NOT expose the terminal tool to the review model.
	if len(provider.requests) < 2 {
		t.Fatalf("expected at least 2 LLM requests (tool call + final), got %d", len(provider.requests))
	}
	sawSkillManage := false
	for _, tool := range provider.requests[0].Tools {
		if tool.Name == "skill_manage" {
			sawSkillManage = true
		}
		if tool.Name == "terminal" {
			t.Fatalf("restricted review backend leaked non-allowlisted tool: %+v", provider.requests[0].Tools)
		}
	}
	if !sawSkillManage {
		t.Fatalf("skill_manage not exposed to review agent: %+v", provider.requests[0].Tools)
	}

	// The create must have SUCCEEDED through the allowlist (not the
	// "background review cannot use tool" refusal): the tool result replayed
	// to the model must be the real skill_manage success message.
	var toolMsg *llm.Message
	for i := range provider.requests[1].Messages {
		if provider.requests[1].Messages[i].Role == "tool" {
			toolMsg = &provider.requests[1].Messages[i]
			break
		}
	}
	if toolMsg == nil {
		t.Fatalf("second request did not include a tool result: %+v", provider.requests[1].Messages)
	}
	if toolMsg.Name != "skill_manage" || strings.Contains(toolMsg.Content, "background review cannot use tool") {
		t.Fatalf("unexpected tool result message: %+v", *toolMsg)
	}
	if !strings.Contains(toolMsg.Content, "created") {
		t.Fatalf("skill_manage create did not succeed: %q", toolMsg.Content)
	}

	// Durable effect 1: the skill exists on disk under the tenant skills root.
	dir, err := getSkillsDir("default")
	if err != nil {
		t.Fatalf("skills dir: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "review-flow", "SKILL.md"))
	if err != nil {
		t.Fatalf("created skill file missing: %v", err)
	}
	if !strings.Contains(string(data), "Inspect the diff") {
		t.Fatalf("skill content not persisted:\n%s", data)
	}

	// Durable effect 2: source metadata is agent-created.
	usage, err := LoadSkillUsage("default")
	if err != nil {
		t.Fatalf("load usage: %v", err)
	}
	if usage["review-flow"].Source != SkillSourceAgentCreated {
		t.Fatalf("expected agent-created source, got %+v", usage["review-flow"])
	}

	// Durable effect 3: a learning-audit create record exists.
	changes, err := ListSkillLearningChanges("default", "review-flow", 20)
	if err != nil {
		t.Fatalf("list learning changes: %v", err)
	}
	foundCreate := false
	for _, c := range changes {
		if c.Action == "create" && c.Kind == "skill" && c.Source == SkillSourceAgentCreated {
			foundCreate = true
		}
	}
	if !foundCreate {
		t.Fatalf("no create learning-audit record for review-flow: %+v", changes)
	}
}
