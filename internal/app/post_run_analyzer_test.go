package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/httpapi"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/modelruntime"
	"selfmind/internal/platform/config"
	"selfmind/internal/promptassets"
)

type postRunProviderStub struct {
	content        string
	response       *llm.ChatResponse
	calls          int
	streamCalls    int
	err            error
	requests       []llm.ChatRequest
	streamRequests []llm.ChatRequest
	streamEvents   []llm.StreamEvent
	responses      []*llm.ChatResponse
}

func TestPostRunAnalyzerUsesPinnedPromptRevision(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "background", "memory_extract.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("## Post-run Analysis\n\nOLD-PINNED-GUIDANCE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldSnapshot, err := promptassets.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := promptassets.SaveRevision(oldSnapshot); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("## Post-run Analysis\n\nNEW-CURRENT-GUIDANCE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := promptassets.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	provider := &postRunProviderStub{content: `{"task_decision":"KEEP","memory_decisions":[]}`}
	analyzer := &llmPostRunAnalyzer{provider: provider, prompts: current}
	if _, err := analyzer.Analyze(context.Background(), httpapi.PostRunAnalysisRequest{
		RunID: "run-pinned", PromptSnapshotHash: oldSnapshot.Hash(),
	}); err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("provider calls = %d", len(provider.requests))
	}
	system := provider.requests[0].SystemPrompt
	if !strings.Contains(system, "OLD-PINNED-GUIDANCE") || strings.Contains(system, "NEW-CURRENT-GUIDANCE") {
		t.Fatalf("system prompt did not use the pinned revision:\n%s", system)
	}
}

func (p *postRunProviderStub) ChatCompletion(context.Context, []llm.Message) (string, error) {
	return p.content, nil
}

func (p *postRunProviderStub) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	return p.chat(req)
}

func (p *postRunProviderStub) chat(req llm.ChatRequest) (*llm.ChatResponse, error) {
	p.calls++
	p.requests = append(p.requests, req)
	if p.err != nil {
		return nil, p.err
	}
	if len(p.responses) > 0 {
		resp := p.responses[0]
		p.responses = p.responses[1:]
		return resp, nil
	}
	if p.response != nil {
		return p.response, nil
	}
	return &llm.ChatResponse{Content: p.content}, nil
}

func TestPostRunAnalyzerAcceptsObjectTaskDecision(t *testing.T) {
	got, err := decodePostRunAnalysis(`{"task_decision":{"action":"MOVE","task_id":"task-42"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if got.TaskDecision != "MOVE:task-42" {
		t.Fatalf("task decision = %q", got.TaskDecision)
	}
}

func TestPostRunAnalyzerAcceptsObjectNewDecision(t *testing.T) {
	got, err := decodePostRunAnalysis(`{"task_decision":{"action":"NEW","title":"Independent access review"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if got.TaskDecision != "NEW:Independent access review" {
		t.Fatalf("task decision = %q", got.TaskDecision)
	}
}

func TestPostRunAnalyzerNormalizesTaskReferences(t *testing.T) {
	got, err := decodePostRunAnalysis(`{"task_decision":"KEEP","task_references":[{"class":"literal","value":"RUQX-42","confidence":0.9},{"class":"literal","value":"ruqx-42","confidence":0.8},{"class":"unknown","value":"ignored","confidence":1}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.TaskReferences) != 1 || got.TaskReferences[0].Value != "RUQX-42" {
		t.Fatalf("task references=%+v", got.TaskReferences)
	}
}

func TestPostRunAnalyzerRetriesTruncatedContractOnce(t *testing.T) {
	provider := &postRunProviderStub{responses: []*llm.ChatResponse{
		{Content: `{"task_decision":`, FinishReason: "length"},
		{Content: `{"task_decision":"KEEP"}`, FinishReason: "stop"},
	}}
	analyzer := &llmPostRunAnalyzer{provider: provider, maxTokens: 1024, batchMaxTokens: 4096, contractRouteID: "contract:test"}
	got, err := analyzer.Analyze(context.Background(), httpapi.PostRunAnalysisRequest{RunID: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.TaskDecision != "KEEP" || provider.calls != 2 {
		t.Fatalf("analysis=%+v calls=%d", got, provider.calls)
	}
	if provider.requests[0].MaxTokens != 1024 || provider.requests[1].MaxTokens != 2048 {
		t.Fatalf("max tokens = %d then %d", provider.requests[0].MaxTokens, provider.requests[1].MaxTokens)
	}
	if got := provider.requests[0].Options["reasoning_effort"]; got != maintenanceReasoningEffort {
		t.Fatalf("reasoning effort = %#v, want %q", got, maintenanceReasoningEffort)
	}
}

func TestPostRunAnalyzerRetriesEmptyTruncatedContractOnce(t *testing.T) {
	provider := &postRunProviderStub{responses: []*llm.ChatResponse{
		{Content: "", FinishReason: "length", Usage: llm.UsageStats{OutputTokens: 8192}},
		{Content: `{"task_decision":"KEEP"}`, FinishReason: "stop"},
	}}
	analyzer := &llmPostRunAnalyzer{provider: provider, maxTokens: 4096, batchMaxTokens: 16384, contractRouteID: "contract:test"}
	got, err := analyzer.Analyze(context.Background(), httpapi.PostRunAnalysisRequest{RunID: "run-empty-truncated"})
	if err != nil {
		t.Fatal(err)
	}
	if got.TaskDecision != "KEEP" || provider.calls != 2 {
		t.Fatalf("analysis=%+v calls=%d", got, provider.calls)
	}
	if provider.requests[0].MaxTokens != 4096 || provider.requests[1].MaxTokens != 8192 {
		t.Fatalf("max tokens = %d then %d", provider.requests[0].MaxTokens, provider.requests[1].MaxTokens)
	}
}

func TestPostRunAnalyzerContractCircuitStopsQueuedRepeatCalls(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	provider := &postRunProviderStub{content: `{"task_decision":`}
	analyzer := &llmPostRunAnalyzer{
		provider: provider, maxTokens: 1024, batchMaxTokens: 4096,
		contractRouteID: "contract:test", controlStore: store,
		routeTenantID: "default", routeProvider: "deepseek", routeModel: "deepseek-v4-flash",
	}
	if _, err := analyzer.Analyze(context.Background(), httpapi.PostRunAnalysisRequest{RunID: "run-1"}); err == nil {
		t.Fatal("first malformed contract should fail")
	}
	if provider.calls != 2 {
		t.Fatalf("first contract calls = %d, want one adaptive retry", provider.calls)
	}
	if _, err := analyzer.Analyze(context.Background(), httpapi.PostRunAnalysisRequest{RunID: "run-2"}); err == nil {
		t.Fatal("open contract circuit should reject the next job")
	}
	if provider.calls != 2 {
		t.Fatalf("open circuit issued another provider call: %d", provider.calls)
	}
	health, err := store.GetProviderRouteHealth(context.Background(), "default", "contract:test")
	if err != nil || health == nil || health.State != control.ProviderRouteOpen || health.FailureClass != "output_contract" {
		t.Fatalf("contract health=%+v err=%v", health, err)
	}
}

func (p *postRunProviderStub) StreamChat(_ context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.streamCalls++
	p.streamRequests = append(p.streamRequests, req)
	ch := make(chan llm.StreamEvent, len(p.streamEvents))
	for _, event := range p.streamEvents {
		ch <- event
	}
	close(ch)
	return ch, nil
}

func TestMaintenanceProviderChainOverridesAuxiliaryReasoning(t *testing.T) {
	provider := &postRunProviderStub{content: `{"task_decision":"KEEP"}`}
	chain := &maintenanceProviderChain{providers: []namedMaintenanceProvider{{
		role: llm.RoleBackgroundReview, provider: provider,
	}}}

	if _, err := chain.Chat(context.Background(), llm.ChatRequest{Options: map[string]interface{}{
		"reasoning_effort": "high",
	}}); err != nil {
		t.Fatal(err)
	}
	if got := provider.requests[0].Options["reasoning_effort"]; got != maintenanceReasoningEffort {
		t.Fatalf("maintenance reasoning = %#v, want %q", got, maintenanceReasoningEffort)
	}

	provider.streamEvents = []llm.StreamEvent{{Content: "ok"}}
	stream, err := chain.StreamChat(context.Background(), llm.ChatRequest{Options: map[string]interface{}{
		"reasoning_effort": "xhigh",
	}})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	if got := provider.streamRequests[0].Options["reasoning_effort"]; got != maintenanceReasoningEffort {
		t.Fatalf("stream maintenance reasoning = %#v, want %q", got, maintenanceReasoningEffort)
	}
}

func TestMaintenanceProviderChainUsesExplicitFallback(t *testing.T) {
	primary := &postRunProviderStub{err: errors.New("403 quota exhausted")}
	fallback := &postRunProviderStub{content: `{"task_decision":"KEEP"}`}
	chain := &maintenanceProviderChain{providers: []namedMaintenanceProvider{
		{role: llm.RoleMemoryExtract, provider: primary},
		{role: llm.RoleBackgroundReview, provider: fallback},
	}}
	resp, err := chain.Chat(context.Background(), llm.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Content == "" || primary.calls != 1 || fallback.calls != 1 {
		t.Fatalf("resp=%+v primary_calls=%d fallback_calls=%d", resp, primary.calls, fallback.calls)
	}
}

func TestMaintenanceProviderChainFallsBackOnEmptyResponse(t *testing.T) {
	primary := &postRunProviderStub{content: "   "}
	fallback := &postRunProviderStub{content: `{"task_decision":"KEEP"}`}
	chain := &maintenanceProviderChain{providers: []namedMaintenanceProvider{
		{role: llm.RoleMemoryExtract, provider: primary},
		{role: llm.RoleBackgroundReview, provider: fallback},
	}}

	resp, err := chain.Chat(context.Background(), llm.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || strings.TrimSpace(resp.Content) == "" || primary.calls != 1 || fallback.calls != 1 {
		t.Fatalf("resp=%+v primary_calls=%d fallback_calls=%d", resp, primary.calls, fallback.calls)
	}
}

func TestMaintenanceProviderChainReturnsFirstTruncatedEmptyResponseForAdaptiveRetry(t *testing.T) {
	primary := &postRunProviderStub{response: &llm.ChatResponse{
		FinishReason: "length",
		Usage:        llm.UsageStats{OutputTokens: 8192},
	}}
	fallback := &postRunProviderStub{content: `{"task_decision":"KEEP"}`}
	chain := &maintenanceProviderChain{providers: []namedMaintenanceProvider{
		{role: llm.RoleMemoryExtract, provider: primary},
		{role: llm.RoleBackgroundReview, provider: fallback},
	}}

	resp, err := chain.Chat(context.Background(), llm.ChatRequest{Options: map[string]interface{}{
		"maintenance_contract_attempt": 1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.FinishReason != "length" || primary.calls != 1 || fallback.calls != 0 {
		t.Fatalf("resp=%+v primary_calls=%d fallback_calls=%d", resp, primary.calls, fallback.calls)
	}
}

func TestMaintenanceProviderChainFallsBackAfterAdaptiveRetryStillTruncates(t *testing.T) {
	primary := &postRunProviderStub{response: &llm.ChatResponse{
		FinishReason: "max_tokens",
		Usage:        llm.UsageStats{OutputTokens: 16384},
	}}
	fallback := &postRunProviderStub{content: `{"task_decision":"KEEP"}`}
	chain := &maintenanceProviderChain{providers: []namedMaintenanceProvider{
		{role: llm.RoleMemoryExtract, provider: primary},
		{role: llm.RoleBackgroundReview, provider: fallback},
	}}

	resp, err := chain.Chat(context.Background(), llm.ChatRequest{Options: map[string]interface{}{
		"maintenance_contract_attempt": 2,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || strings.TrimSpace(resp.Content) == "" || primary.calls != 1 || fallback.calls != 1 {
		t.Fatalf("resp=%+v primary_calls=%d fallback_calls=%d", resp, primary.calls, fallback.calls)
	}
}

func TestMaintenanceProviderChainAcceptsToolCallOnlyResponse(t *testing.T) {
	primary := &postRunProviderStub{response: &llm.ChatResponse{ToolCalls: []llm.ToolCall{{
		ID: "call-1", Function: "review", Args: `{}`,
	}}}}
	fallback := &postRunProviderStub{content: `{"task_decision":"KEEP"}`}
	chain := &maintenanceProviderChain{providers: []namedMaintenanceProvider{
		{role: llm.RoleMemoryExtract, provider: primary},
		{role: llm.RoleBackgroundReview, provider: fallback},
	}}

	resp, err := chain.Chat(context.Background(), llm.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || len(resp.ToolCalls) != 1 || primary.calls != 1 || fallback.calls != 0 {
		t.Fatalf("resp=%+v primary_calls=%d fallback_calls=%d", resp, primary.calls, fallback.calls)
	}
}

func TestMaintenanceCredentialIdentityKeepsDynamicOAuthRouteStable(t *testing.T) {
	first := &modelruntime.Runtime{
		APIKey: "access-token-1", CredentialSource: "auth.json:minimax-oauth",
		TokenGetter: func() string { return "access-token-1" },
	}
	second := &modelruntime.Runtime{
		APIKey: "access-token-2", CredentialSource: "auth.json:minimax-oauth",
		TokenGetter: func() string { return "access-token-2" },
	}
	if got, want := maintenanceCredentialIdentity(first), maintenanceCredentialIdentity(second); got != want {
		t.Fatalf("dynamic route identity changed after refresh: %q != %q", got, want)
	}

	staticA := &modelruntime.Runtime{APIKey: "key-a", CredentialSource: "config"}
	staticB := &modelruntime.Runtime{APIKey: "key-b", CredentialSource: "config"}
	if maintenanceCredentialIdentity(staticA) == maintenanceCredentialIdentity(staticB) {
		t.Fatal("different static credentials must not share a quota circuit")
	}
}

func TestMaintenanceProviderChainKeepsEmptyResponsesRetryable(t *testing.T) {
	chain := &maintenanceProviderChain{providers: []namedMaintenanceProvider{
		{role: llm.RoleMemoryExtract, provider: &postRunProviderStub{content: " "}},
		{role: llm.RoleBackgroundReview, provider: &postRunProviderStub{}},
	}}

	_, err := chain.Chat(context.Background(), llm.ChatRequest{})
	if err == nil {
		t.Fatal("all-empty provider chain must fail")
	}
	if !llm.IsRetryableError(err) {
		t.Fatalf("empty output can recover with a smaller batch or larger budget: %v", err)
	}
}

func TestMaintenanceProviderChainKeepsFatalOnlyFailuresNonRetryable(t *testing.T) {
	chain := &maintenanceProviderChain{providers: []namedMaintenanceProvider{
		{role: llm.RoleMemoryExtract, provider: &postRunProviderStub{err: errors.New("HTTP 401 unauthorized")}},
		{role: llm.RoleBackgroundReview, provider: &postRunProviderStub{err: errors.New("HTTP 403 quota exhausted")}},
	}}
	_, err := chain.Chat(context.Background(), llm.ChatRequest{})
	if err == nil || llm.IsRetryableError(err) {
		t.Fatalf("fatal-only provider chain must fail fast: %v", err)
	}
}

func TestMaintenanceProviderChainOpensQuotaCircuitAfterOneRequest(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	provider := &postRunProviderStub{err: &llm.ProviderError{
		Provider: "kimi-coding", Class: llm.ProviderErrorQuota, StatusCode: 403,
		Message: "usage limit reached",
	}}
	chain := &maintenanceProviderChain{
		providers: []namedMaintenanceProvider{{
			role: llm.RoleMemoryExtract, provider: provider,
			route: maintenanceRouteIdentity{ID: "route-kimi", Provider: "kimi-coding", Model: "kimi-for-coding"},
		}},
		control: store, tenantID: "tenant", probeInitial: time.Hour, probeMax: 4 * time.Hour, probeLease: time.Minute,
	}
	if _, err := chain.Chat(context.Background(), llm.ChatRequest{}); err == nil || !llm.IsQuotaError(err) {
		t.Fatalf("first call error = %v", err)
	}
	if _, err := chain.Chat(context.Background(), llm.ChatRequest{}); err == nil || !llm.IsQuotaError(err) {
		t.Fatalf("open-circuit call error = %v", err)
	}
	if provider.calls != 1 {
		t.Fatalf("physical provider calls = %d, want 1", provider.calls)
	}
}

func TestMaintenanceProviderChainOpensSoftCircuitAfterOutputExhaustion(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	provider := &postRunProviderStub{err: &llm.ProviderError{
		Provider: "kimi-coding", Class: llm.ProviderErrorEmptyResponse,
		Message: "HTTP 200 response contained no text or tool use", StopReason: "max_tokens",
		Usage: llm.UsageStats{InputTokens: 1200, OutputTokens: 3072},
	}}
	chain := &maintenanceProviderChain{
		providers: []namedMaintenanceProvider{{
			role: llm.RoleMemoryExtract, provider: provider,
			route: maintenanceRouteIdentity{ID: "route-kimi-soft", Provider: "kimi-coding", Model: "kimi-for-coding"},
		}},
		control: store, tenantID: "tenant", softProbeInitial: time.Hour, softProbeMax: time.Hour, probeLease: time.Minute,
	}
	if _, err := chain.Chat(context.Background(), llm.ChatRequest{Options: map[string]any{"maintenance_batch_size": 3}}); err == nil {
		t.Fatal("first output-exhausted request must fail")
	}
	if _, err := chain.Chat(context.Background(), llm.ChatRequest{}); err == nil {
		t.Fatal("open soft circuit must skip the physical provider")
	}
	if provider.calls != 1 {
		t.Fatalf("physical provider calls = %d, want 1", provider.calls)
	}
	usage, err := store.MaintenanceProviderUsageSince(context.Background(), "tenant", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 || usage[0].Failed != 1 || usage[0].CircuitOpen != 1 || usage[0].OutputTokens != 3072 {
		t.Fatalf("maintenance provider usage = %+v", usage)
	}
}

func TestMaintenanceProviderChainObservesQuotaInsideStream(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	provider := &postRunProviderStub{streamEvents: []llm.StreamEvent{{Err: &llm.ProviderError{
		Provider: "kimi-coding", Class: llm.ProviderErrorQuota, StatusCode: 403,
		Message: "usage limit reached inside stream",
	}}}}
	chain := &maintenanceProviderChain{
		providers: []namedMaintenanceProvider{{
			role: llm.RoleBackgroundReview, provider: provider,
			route: maintenanceRouteIdentity{ID: "route-kimi-stream", Provider: "kimi-coding", Model: "kimi-for-coding"},
		}},
		control: store, tenantID: "tenant", probeInitial: time.Hour, probeMax: 4 * time.Hour, probeLease: time.Minute,
	}
	stream, err := chain.StreamChat(context.Background(), llm.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	event := <-stream
	if event.Err == nil || !llm.IsQuotaError(event.Err) {
		t.Fatalf("stream error = %v", event.Err)
	}
	if _, err := chain.StreamChat(context.Background(), llm.ChatRequest{}); err == nil || !llm.IsQuotaError(err) {
		t.Fatalf("open-circuit stream error = %v", err)
	}
	if provider.streamCalls != 1 {
		t.Fatalf("physical stream calls = %d, want 1", provider.streamCalls)
	}
}

func TestPostRunAnalyzerCombinesDecisionAndFactPersistence(t *testing.T) {
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mem := memory.NewMemoryManager(provider)
	model := &postRunProviderStub{content: `{
		"task_decision":"TITLE:Post-run maintenance",
		"user_facts":["User prefers concise engineering summaries"],
		"memory_facts":["The active repository uses Go"]
	}`}
	analyzer := &llmPostRunAnalyzer{provider: model, memory: mem}
	req := httpapi.PostRunAnalysisRequest{
		Prompt: "analyze", TenantID: "tenant", PersonID: "person",
		WorkspaceID: "workspace", TaskID: "task", RunID: "run",
	}

	got, err := analyzer.Analyze(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.TaskDecision != "TITLE:Post-run maintenance" || model.calls != 1 {
		t.Fatalf("analysis=%+v calls=%d", got, model.calls)
	}
	if err := analyzer.Apply(context.Background(), req, got); err != nil {
		t.Fatal(err)
	}
	// Background learning must land in the person partition — the one the
	// foreground agent reads — never in the control tenant partition (the
	// 2026-07-17 partition-split regression).
	userFacts, err := mem.GetFacts(context.Background(), "person", "user")
	if err != nil || len(userFacts) != 1 {
		t.Fatalf("user facts=%+v err=%v", userFacts, err)
	}
	if userFacts[0].CreatedFromRun != "run" || userFacts[0].Scope != "global" {
		t.Fatalf("user fact metadata=%+v", userFacts[0])
	}
	workspaceFacts, err := mem.GetFacts(context.Background(), "person", "memory")
	if err != nil || len(workspaceFacts) != 1 {
		t.Fatalf("memory facts=%+v err=%v", workspaceFacts, err)
	}
	if workspaceFacts[0].Scope != "workspace:workspace" {
		t.Fatalf("workspace fact metadata=%+v", workspaceFacts[0])
	}
	if tenantFacts, err := mem.GetFacts(context.Background(), "tenant", "user"); err != nil || len(tenantFacts) != 0 {
		t.Fatalf("control tenant partition must stay empty, got %+v err=%v", tenantFacts, err)
	}

	// Re-analyzing the same durable facts must not duplicate memory rows —
	// and the duplicate observation is corroborating evidence, so the stored
	// fact must be REINFORCED (confidence up, verification time refreshed),
	// never silently dropped.
	firstConfidence := userFacts[0].Confidence
	firstVerified := userFacts[0].LastVerifiedAt
	replayed, err := analyzer.Analyze(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := analyzer.Apply(context.Background(), req, replayed); err != nil {
		t.Fatal(err)
	}
	userFacts, _ = mem.GetFacts(context.Background(), "person", "user")
	workspaceFacts, _ = mem.GetFacts(context.Background(), "person", "memory")
	if len(userFacts) != 1 || len(workspaceFacts) != 1 {
		t.Fatalf("duplicates stored: user=%d memory=%d", len(userFacts), len(workspaceFacts))
	}
	if userFacts[0].Confidence != firstConfidence {
		t.Fatalf("same-run replay must not reinforce twice: %v -> %v", firstConfidence, userFacts[0].Confidence)
	}
	if userFacts[0].LastVerifiedAt.Before(firstVerified) {
		t.Fatalf("reinforcement must not move last_verified_at backwards: %v -> %v", firstVerified, userFacts[0].LastVerifiedAt)
	}
}

func TestDecodePostRunAnalysisRejectsNonJSON(t *testing.T) {
	if _, err := decodePostRunAnalysis("KEEP"); err == nil {
		t.Fatal("non-JSON analyzer output must be rejected")
	}
}

func TestPostRunAnalyzerBatchesProviderCallAndKeysResultsByRun(t *testing.T) {
	model := &postRunProviderStub{content: `{
		"runs":[
			{"run_id":"run-2","task_decision":"INBOX"},
			{"run_id":"run-1","task_decision":"TITLE:Release checks"}
		]
	}`}
	analyzer := &llmPostRunAnalyzer{provider: model}
	results, err := analyzer.AnalyzeBatch(context.Background(), []httpapi.PostRunAnalysisRequest{
		{Prompt: "first", TenantID: "tenant", PersonID: "person", WorkspaceID: "ws", TaskID: "task-1", RunID: "run-1"},
		{Prompt: "second", TenantID: "tenant", PersonID: "person", WorkspaceID: "ws", TaskID: "task-2", RunID: "run-2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if model.calls != 1 {
		t.Fatalf("provider calls = %d", model.calls)
	}
	if got := model.requests[0].MaxTokens; got != postRunAnalyzerMaxTokens {
		t.Fatalf("batch max tokens = %d, want %d", got, postRunAnalyzerMaxTokens)
	}
	if results["run-1"].TaskDecision != "TITLE:Release checks" || results["run-2"].TaskDecision != "INBOX" {
		t.Fatalf("results = %+v", results)
	}
}

func TestPostRunBatchSystemContractIsSelfContained(t *testing.T) {
	for _, want := range []string{
		"SKIP: temporary, speculative, secret, episodic, or already fully represented",
		"REINFORCE, SUPERSEDE, and CONFLICT must set ref to an id from that run's nearby list",
		`target is "user" for user preferences/identity and "memory" for workspace facts and conventions`,
		"Never carry a ref or decision across runs",
		"Write memory content in the language of its supporting user statement or durable result",
		"SelfMind-generated task decision rules",
	} {
		if !strings.Contains(postRunBatchAnalyzerSystemPrompt, want) {
			t.Fatalf("batch maintenance contract missing %q:\n%s", want, postRunBatchAnalyzerSystemPrompt)
		}
	}
	if strings.Contains(postRunBatchAnalyzerSystemPrompt, "same semantics described in each run") {
		t.Fatal("batch maintenance contract still delegates locked decision semantics to untrusted run data")
	}
}

func TestConfiguredPostRunAnalyzerDeduplicatesEquivalentRoleRoutes(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{Provider: "kimi-coding", Default: "kimi-for-coding"},
		ProviderProfiles: map[string]config.ProviderEndpoint{
			"kimi-coding": {APIKey: "sk-kimi-test"},
		},
		Models: config.ModelsConfig{Roles: map[string]config.ModelRoleConfig{
			"memory_extract":    {Provider: "kimi-coding", Model: "kimi-for-coding"},
			"background_review": {Provider: "kimi-coding", Model: "another-maintenance-model", MaxTokens: 1024},
		}},
		Tasks: config.TaskConfig{MaintenanceFallbackRoles: []string{"background_review"}},
	}
	cfg.Normalize()
	analyzer, ok := NewConfiguredPostRunAnalyzer(nil, cfg, "tenant", nil, nil).(*llmPostRunAnalyzer)
	if !ok || analyzer == nil {
		t.Fatal("configured analyzer was not built")
	}
	if _, chained := analyzer.provider.(*maintenanceProviderChain); chained {
		t.Fatal("roles sharing one Kimi endpoint and credential must not be called repeatedly")
	}
}

func TestMaintenanceRouteKeyIgnoresRequestTuningButSeparatesCredentials(t *testing.T) {
	base := &config.Config{
		Model:            config.ModelConfig{Provider: "kimi-coding", Default: "kimi-for-coding"},
		ProviderProfiles: map[string]config.ProviderEndpoint{"kimi-coding": {APIKey: "key-a"}},
		Models: config.ModelsConfig{Roles: map[string]config.ModelRoleConfig{
			"memory_extract":    {Provider: "kimi-coding", Model: "kimi-for-coding", MaxTokens: 4096},
			"background_review": {Provider: "kimi-coding", Model: "other", MaxTokens: 1024, ReasoningEffort: "high"},
		}},
	}
	base.Normalize()
	first := maintenanceRoleRouteKey(base, llm.RoleMemoryExtract)
	second := maintenanceRoleRouteKey(base, llm.RoleBackgroundReview)
	if first == "" || first != second {
		t.Fatalf("same physical route should share key: %q != %q", first, second)
	}
	base.ProviderProfiles["kimi-coding"] = config.ProviderEndpoint{APIKey: "key-b"}
	if changed := maintenanceRoleRouteKey(base, llm.RoleMemoryExtract); changed == first {
		t.Fatal("credential change must produce a fresh quota route")
	}
}

func TestConfiguredMaintenanceRouteIDsUseStableSemanticRoles(t *testing.T) {
	cfg := &config.Config{
		Auth:  config.AuthConfig{CredentialsFile: filepath.Join(t.TempDir(), "auth.json")},
		Model: config.ModelConfig{Provider: "openai", Default: "gpt-main"},
		ProviderProfiles: map[string]config.ProviderEndpoint{
			"memory-provider": {APIKey: "memory-key", BaseURL: "https://memory.example/v1", Protocol: "openai_compatible"},
			"review-provider": {APIKey: "review-key", BaseURL: "https://review.example/v1", Protocol: "openai_compatible"},
		},
		Models: config.ModelsConfig{Roles: map[string]config.ModelRoleConfig{
			"memory_extract":    {Provider: "memory-provider", Model: "memory-model"},
			"background_review": {Provider: "review-provider", Model: "review-model"},
		}},
		Memory: config.MemoryConfig{Governance: config.MemoryGovernanceConfig{Enabled: true}},
	}
	cfg.Normalize()
	got := configuredMaintenanceRouteIDs(cfg)
	want := maintenanceRouteSet(cfg, llm.RoleMemoryExtract, llm.RoleBackgroundReview)
	if len(got) != len(want) {
		t.Fatalf("route ids = %v, want %d distinct routes", got, len(want))
	}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("unexpected route id %q in %v", id, got)
		}
	}
}

func TestConfiguredMaintenanceRouteIDsUseAuxiliaryAndExplicitOverrides(t *testing.T) {
	cfg := &config.Config{
		ProviderProfiles: map[string]config.ProviderEndpoint{
			"aux-provider":      {APIKey: "aux-key", BaseURL: "https://aux.example/v1", Protocol: "openai_compatible"},
			"override-provider": {APIKey: "override-key", BaseURL: "https://override.example/v1", Protocol: "openai_compatible"},
		},
		Models: config.ModelsConfig{
			Auxiliary: config.ModelSelectionConfig{Provider: "aux-provider", Model: "aux-model"},
			Roles: map[string]config.ModelRoleConfig{
				"background_review": {Provider: "override-provider", Model: "review-model"},
			},
		},
		Tasks: config.TaskConfig{MaintenanceFallbackRoles: []string{"background_review", "fast_classifier"}},
	}
	cfg.Normalize()

	got := configuredMaintenanceRouteIDs(cfg)
	want := maintenanceRouteSet(cfg, llm.RoleMemoryExtract, llm.RoleBackgroundReview)
	if len(got) != len(want) {
		t.Fatalf("route ids = %v, want %d physical routes", got, len(want))
	}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("unexpected route id %q in %v", id, got)
		}
	}
}

func maintenanceRouteSet(cfg *config.Config, roles ...llm.ModelRole) map[string]bool {
	out := make(map[string]bool, len(roles)*2)
	for _, role := range roles {
		route := maintenanceRoleRouteIdentity(cfg, role)
		if route.ID != "" {
			out[route.ID] = true
		}
		if route.ContractID != "" {
			out[route.ContractID] = true
		}
	}
	return out
}

func TestKimiAuxiliaryRolesUseProviderDefaultTransport(t *testing.T) {
	cfg := &config.Config{
		Model: config.ModelConfig{Provider: "kimi-coding", Default: "kimi-for-coding"},
		ProviderProfiles: map[string]config.ProviderEndpoint{
			"kimi-coding": {APIKey: "sk-kimi-test"},
		},
	}
	cfg.Normalize()
	roleCfg := config.ModelRoleConfig{Provider: "kimi-coding", Model: "kimi-for-coding"}
	rt, err := modelruntime.NewResolver(cfg).Resolve(context.Background(), roleProviderSelection(llm.RoleMemoryExtract, "kimi-coding", roleCfg))
	if err != nil {
		t.Fatal(err)
	}
	if rt.Protocol != modelruntime.ProtocolAnthropic || rt.BaseURL != "https://api.kimi.com/coding" {
		t.Fatalf("auxiliary runtime = protocol %q base %q", rt.Protocol, rt.BaseURL)
	}

	roleCfg.Protocol = modelruntime.ProtocolOpenAICompatible
	rt, err = modelruntime.NewResolver(cfg).Resolve(context.Background(), roleProviderSelection(llm.RoleMemoryExtract, "kimi-coding", roleCfg))
	if err != nil {
		t.Fatal(err)
	}
	if rt.Protocol != modelruntime.ProtocolOpenAICompatible || rt.BaseURL != "https://api.kimi.com/coding/v1" {
		t.Fatalf("explicit protocol override = protocol %q base %q", rt.Protocol, rt.BaseURL)
	}
}

// maintenanceTwoLevelConfig builds a config whose maintenance role and
// models.auxiliary sit on independent endpoints and credentials.
func maintenanceTwoLevelConfig() *config.Config {
	cfg := &config.Config{
		ProviderProfiles: map[string]config.ProviderEndpoint{
			"role-provider": {APIKey: "role-key", BaseURL: "https://role.example/v1", Protocol: "openai_compatible"},
			"aux-provider":  {APIKey: "aux-key", BaseURL: "https://aux.example/v1", Protocol: "openai_compatible"},
		},
		Models: config.ModelsConfig{
			Auxiliary: config.ModelSelectionConfig{Provider: "aux-provider", Model: "aux-model"},
			Roles: map[string]config.ModelRoleConfig{
				"memory_extract": {Provider: "role-provider", Model: "role-model"},
			},
		},
	}
	cfg.Normalize()
	return cfg
}

func TestMaintenanceCandidateSlotsAppendAuxiliaryFloor(t *testing.T) {
	cfg := maintenanceTwoLevelConfig()
	slots := maintenanceCandidateSlots(cfg, llm.RoleMemoryExtract)
	if len(slots) != 2 {
		t.Fatalf("slots = %d, want the role plus the auxiliary floor", len(slots))
	}
	if slots[0].slot != string(llm.RoleMemoryExtract) || slots[0].roleCfg.Provider != "role-provider" {
		t.Fatalf("first slot = %+v, want the role's own configuration", slots[0])
	}
	if slots[1].slot != auxiliaryFloorSlot || slots[1].roleCfg.Provider != "aux-provider" {
		t.Fatalf("second slot = %+v, want the models.auxiliary floor", slots[1])
	}
	// The floor serves the role that needed it, so telemetry keeps attributing
	// the call to that work rather than to a synthetic role.
	if slots[1].role != llm.RoleMemoryExtract {
		t.Fatalf("floor role = %q, want the logical role it serves", slots[1].role)
	}
}

// A role without its own override resolves to models.auxiliary, so the floor is
// the same physical route. The chain must collapse instead of pretending the
// installation has a second provider.
func TestMaintenanceCandidateSlotsCollapseOntoOneRoute(t *testing.T) {
	cfg := maintenanceTwoLevelConfig()
	slots := maintenanceCandidateSlots(cfg, llm.RoleBackgroundReview)
	if len(slots) != 2 {
		t.Fatalf("slots = %d, want the role plus the auxiliary floor", len(slots))
	}
	first, _ := maintenanceRouteIdentityFor(cfg, slots[0].role, slots[0].roleCfg)
	second, _ := maintenanceRouteIdentityFor(cfg, slots[1].role, slots[1].roleCfg)
	if first.ID == "" || first.ID != second.ID {
		t.Fatalf("routes = %q/%q, want one shared physical route", first.ID, second.ID)
	}
}

func TestMaintenanceCandidateSlotsKeepLegacyFallbackRoles(t *testing.T) {
	cfg := maintenanceTwoLevelConfig()
	cfg.ProviderProfiles["legacy-provider"] = config.ProviderEndpoint{
		APIKey: "legacy-key", BaseURL: "https://legacy.example/v1", Protocol: "openai_compatible",
	}
	cfg.Models.Roles["maintenance_backup"] = config.ModelRoleConfig{Provider: "legacy-provider", Model: "legacy-model"}
	cfg.Tasks.MaintenanceFallbackRoles = []string{"maintenance_backup"}
	cfg.Normalize()

	slots := maintenanceCandidateSlots(cfg, llm.RoleMemoryExtract)
	if len(slots) != 3 {
		t.Fatalf("slots = %d, want role, legacy hop, and auxiliary floor", len(slots))
	}
	if slots[1].slot != "maintenance_backup" {
		t.Fatalf("legacy hop = %q, want it between the role and the floor", slots[1].slot)
	}
	if slots[2].slot != auxiliaryFloorSlot {
		t.Fatalf("last slot = %q, want the auxiliary floor to stay last", slots[2].slot)
	}
}

func TestDescribeMaintenanceFallbackReportsFloorAndCollapse(t *testing.T) {
	cfg := maintenanceTwoLevelConfig()

	withFloor := DescribeMaintenanceFallback(cfg, string(llm.RoleMemoryExtract))
	if !withFloor.Chained || withFloor.Slot != auxiliaryFloorSlot || withFloor.Provider != "aux-provider" || withFloor.Collapsed {
		t.Fatalf("memory_extract fallback = %+v, want the auxiliary floor", withFloor)
	}

	collapsed := DescribeMaintenanceFallback(cfg, string(llm.RoleBackgroundReview))
	if !collapsed.Chained || collapsed.Provider != "" || !collapsed.Collapsed {
		t.Fatalf("background_review fallback = %+v, want a collapsed chain", collapsed)
	}

	// Roles outside the maintenance chain resolve to exactly one provider and
	// must not be described as having a fallback at all.
	unchained := DescribeMaintenanceFallback(cfg, string(llm.RoleSemanticRecall))
	if unchained.Chained {
		t.Fatalf("semantic_recall fallback = %+v, want no chain", unchained)
	}
}

// The analyzer sizes its output contract from the role's own route. A fallback
// route with a smaller ceiling must be handed its own limit, otherwise it
// returns a truncated empty body that reads as a provider fault.
func TestMaintenanceProviderChainClampsOutputToRouteCeiling(t *testing.T) {
	primary := &postRunProviderStub{err: errors.New("403 quota exhausted")}
	fallback := &postRunProviderStub{content: `{"task_decision":"KEEP"}`}
	chain := &maintenanceProviderChain{providers: []namedMaintenanceProvider{
		{slot: "memory_extract", role: llm.RoleMemoryExtract, provider: primary, maxOutputTokens: 32768},
		{slot: auxiliaryFloorSlot, role: llm.RoleMemoryExtract, provider: fallback, maxOutputTokens: 4096},
	}}

	if _, err := chain.Chat(context.Background(), llm.ChatRequest{MaxTokens: 16000}); err != nil {
		t.Fatal(err)
	}
	if got := primary.requests[0].MaxTokens; got != 16000 {
		t.Fatalf("primary max_tokens = %d, want the request budget left alone", got)
	}
	if got := fallback.requests[0].MaxTokens; got != 4096 {
		t.Fatalf("fallback max_tokens = %d, want the route ceiling", got)
	}
}

func TestMaintenanceProviderChainNeverRaisesOutputBudget(t *testing.T) {
	provider := &postRunProviderStub{content: `{"task_decision":"KEEP"}`}
	chain := &maintenanceProviderChain{providers: []namedMaintenanceProvider{
		{slot: "memory_extract", role: llm.RoleMemoryExtract, provider: provider, maxOutputTokens: 8192},
	}}
	if _, err := chain.Chat(context.Background(), llm.ChatRequest{MaxTokens: 512}); err != nil {
		t.Fatal(err)
	}
	if got := provider.requests[0].MaxTokens; got != 512 {
		t.Fatalf("max_tokens = %d, want a deliberately small budget preserved", got)
	}
}
