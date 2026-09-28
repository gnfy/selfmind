package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/config"
	"selfmind/internal/tools"
)

// recordingTool stands in for a real tool. It records what reached it and
// executes nothing, so a policy regression cannot run the commands below.
type recordingTool struct {
	name, param string
	mu          sync.Mutex
	got         []string
	tenants     []string
}

func (t *recordingTool) Name() string        { return t.name }
func (t *recordingTool) Description() string { return "records its " + t.param }
func (t *recordingTool) Schema() tools.ToolSchema {
	return tools.ToolSchema{Type: "object", Properties: map[string]tools.PropertyDef{t.param: {Type: "string"}}, Required: []string{t.param}}
}
func (t *recordingTool) Execute(args map[string]interface{}) (string, error) {
	value, _ := args[t.param].(string)
	tenant, _ := args["_tenant_id"].(string)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.got = append(t.got, value)
	t.tenants = append(t.tenants, tenant)
	return "recorded", nil
}
func (t *recordingTool) tenantIDs() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.tenants...)
}
func (t *recordingTool) received() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.got...)
}

// scriptedStreamProvider streams its responses in turn, then a final answer.
type scriptedStreamProvider struct {
	mu        sync.Mutex
	responses []llm.ChatResponse
	requests  []llm.ChatRequest
}

func (p *scriptedStreamProvider) recorded() []llm.ChatRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]llm.ChatRequest(nil), p.requests...)
}

func (p *scriptedStreamProvider) next(req llm.ChatRequest) llm.ChatResponse {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	if len(p.responses) == 0 {
		return llm.ChatResponse{Content: "Done.", FinishReason: "stop"}
	}
	resp := p.responses[0]
	p.responses = p.responses[1:]
	return resp
}
func (p *scriptedStreamProvider) ChatCompletion(context.Context, []llm.Message) (string, error) {
	return "Done.", nil
}
func (p *scriptedStreamProvider) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	resp := p.next(req)
	return &resp, nil
}
func (p *scriptedStreamProvider) StreamChat(_ context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	resp := p.next(req)
	ch := make(chan llm.StreamEvent, 2)
	ch <- llm.StreamEvent{Content: resp.Content, ToolCalls: resp.ToolCalls}
	ch <- llm.StreamEvent{FinishReason: resp.FinishReason}
	close(ch)
	return ch, nil
}

// policyParent is a parent dispatcher with the workspace scope and safety
// floor the daemon installs, over recording stand-ins for its tools.
func policyParent(t *testing.T) (*tools.Dispatcher, *recordingTool, *recordingTool) {
	read := &recordingTool{name: "read_file", param: "path"}
	terminal := &recordingTool{name: "terminal", param: "command"}
	parent := tools.NewDispatcher()
	parent.RegisterTool(read)
	parent.RegisterTool(terminal)
	parent.InjectMiddleware(tools.WorkspaceScopeMiddleware())
	parent.InjectMiddleware(tools.SmartApprovalMiddleware(t.TempDir()))
	return parent, read, terminal
}

// parentRunContext is a run's context as the coordinator installs it: the
// run's execution scope, its context key and its trusted invocation scope,
// plus the run's approval handler when one is given.
func parentRunContext(t *testing.T, runID, workspace string, approval ...tools.ToolApprovalHandler) context.Context {
	scope := tools.ExecutionScope{
		PersonID: "person_parent", RunID: runID, WorkspaceRoot: workspace, AllowedRoots: []string{workspace},
	}
	if len(approval) > 0 {
		scope.Approval = approval[0]
	}
	t.Cleanup(tools.SetExecutionScope("person_parent", scope))
	key := tools.ExecutionScopeKeyForRun(runID)
	ctx := tools.WithExecutionScopeKey(context.Background(), key)
	return kernel.WithToolInvocationScope(ctx, kernel.ToolInvocationScope{PersonID: "person_parent", RunID: runID, ExecutionScopeKey: key})
}

// A delegated sub-agent's tool calls meet the parent run's policy: paths
// resolve inside the parent's workspace, escapes are refused, and the safety
// floor holds. The sub-agent's dispatcher once carried no middleware and its
// calls resolved no scope, so all three reached the tool.
func TestDelegatedSubAgentToolsKeepTheParentRunPolicy(t *testing.T) {
	workspace := t.TempDir()
	parent, read, terminal := policyParent(t)
	provider := &scriptedStreamProvider{responses: []llm.ChatResponse{{
		FinishReason: "tool_calls",
		ToolCalls: []llm.ToolCall{
			{ID: "inside", Function: "read_file", Args: `{"path":"notes.txt"}`},
			{ID: "escape", Function: "read_file", Args: `{"path":"/etc/hosts"}`},
			{ID: "floor", Function: "terminal", Args: `{"command":"rm -rf /"}`},
		},
	}}}
	sub := buildDelegateSubBackend(parent, config.DelegationConfig{}, nil, nil, nil, 1)
	if _, _, err := runDelegatedGoal(parentRunContext(t, "run_parent", workspace), sub, provider, nil, 4, 1, "inspect the notes"); err != nil {
		t.Fatal(err)
	}
	if got := read.received(); len(got) != 1 || got[0] != filepath.Join(workspace, "notes.txt") {
		t.Fatalf("read_file received %q, want only the workspace-resolved path", got)
	}
	if got := terminal.received(); len(got) != 0 {
		t.Fatalf("a delegated command passed the safety floor: %q", got)
	}
}

func TestSubAgentBackendsKeepTheParentPolicyChain(t *testing.T) {
	for name, build := range map[string]func(*tools.Dispatcher) kernel.AgentBackend{
		"single goal": func(parent *tools.Dispatcher) kernel.AgentBackend {
			return buildDelegateSubBackend(parent, config.DelegationConfig{}, nil, nil, nil, 1)
		},
		"batch": func(parent *tools.Dispatcher) kernel.AgentBackend {
			return NewMultiAgentHost(parent, nil, nil, 1, 1, 1, 1).buildSubBackend(nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			parent, _, terminal := policyParent(t)
			_, err := build(parent).Dispatch("terminal", map[string]interface{}{"command": "rm -rf /"})
			if err == nil || !strings.Contains(err.Error(), "safety policy") || len(terminal.received()) != 0 {
				t.Fatalf("err=%v received=%q, want the parent's safety floor", err, terminal.received())
			}
		})
	}
}

// A sub-agent is part of the parent's foreground run and runs on the model the
// parent run uses; delegation used to need its own API key and failed without.
func TestDelegationRunsOnTheParentRunModel(t *testing.T) {
	provider := &scriptedStreamProvider{}
	parentAgent := kernel.NewAgent(memory.NewMemoryManager(nil), tools.NewDispatcher(), provider, "helpful", 3, 1, nil)
	cfg := &config.Config{}
	delegate := MakeDelegateFn(tools.NewDispatcher(), cfg.Delegation, nil, delegationModelSource(cfg, nil, "default", parentAgent))
	out, _, err := delegate(context.Background(), "summarise the notes", "", nil)
	if err != nil || !strings.Contains(out, "Done.") || len(provider.recorded()) == 0 {
		t.Fatalf("out=%q err=%v requests=%d, want the sub-agent on the parent's model", out, err, len(provider.recorded()))
	}
	if _, err := delegationModelSource(cfg, nil, "default", nil)(); err == nil {
		t.Fatal("a delegation with no model at all must fail rather than run")
	}
}

func TestDelegationModelOverrideResolvesThroughTheProviderRuntime(t *testing.T) {
	cfg := &config.Config{Delegation: config.DelegationConfig{Provider: "openai", Model: "gpt-delegate", APIKey: "test-key"}}
	provider, err := delegationModelSource(cfg, nil, "default", nil)()
	if err != nil || llm.GetModelName(provider) != "gpt-delegate" {
		t.Fatalf("model=%q err=%v, want the configured override", llm.GetModelName(provider), err)
	}
	cfg.Delegation = config.DelegationConfig{Provider: "no-such-provider", APIKey: "test-key"}
	if _, err := delegationModelSource(cfg, nil, "default", nil)(); err == nil {
		t.Fatal("an unknown delegation provider must fail")
	}
}

// Sub-agents keep no conversation memory, so one delegation never sees another's
// even when the parent's memory persists, and their tool calls act as the
// parent run's person rather than a synthetic tenant.
func TestDelegatedSubAgentsKeepNoConversationAndActAsTheParentPerson(t *testing.T) {
	store, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	workspace := t.TempDir()
	parent, read, _ := policyParent(t)
	provider := &scriptedStreamProvider{responses: []llm.ChatResponse{
		{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{ID: "alpha", Function: "read_file", Args: `{"path":"alpha.txt"}`}}},
		{Content: "ALPHA report", FinishReason: "stop"},
		{Content: "BETA report", FinishReason: "stop"},
		{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{ID: "gamma", Function: "read_file", Args: `{"path":"gamma.txt"}`}}},
		{Content: "GAMMA report", FinishReason: "stop"},
	}}
	parentAgent := kernel.NewAgent(memory.NewMemoryManager(store), parent, provider, "helpful", 3, 1, nil)
	cfg := &config.Config{}
	model := delegationModelSource(cfg, nil, "default", parentAgent)
	ctx := parentRunContext(t, "run_parent", workspace)

	delegate := MakeDelegateFn(parent, cfg.Delegation, nil, model)
	for _, goal := range []string{"report on ALPHA", "report on BETA"} {
		if _, _, err := delegate(ctx, goal, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	requests := provider.recorded()
	if len(requests) != 3 {
		t.Fatalf("requests = %d, want two for ALPHA and one for BETA", len(requests))
	}
	for _, message := range requests[2].Messages {
		if strings.Contains(message.Content, "ALPHA") {
			t.Fatalf("the BETA delegation saw the ALPHA conversation: %q", message.Content)
		}
	}

	batch := MakeDelegateBatchFn(parent, cfg.Delegation, nil, model)
	if results, err := batch(ctx, []tools.DelegateTaskSpec{{Goal: "report on GAMMA"}}); err != nil || len(results) != 1 || results[0].Error != "" {
		t.Fatalf("batch results=%+v err=%v", results, err)
	}
	if got := read.tenantIDs(); len(got) != 2 || got[0] != "person_parent" || got[1] != "person_parent" {
		t.Fatalf("delegated tool calls acted as %q, want the parent run's person", got)
	}
}

// controlLedger is the run ledger as the daemon keeps it, in a control store:
// a tool call ID names one claim in the run, and a second claim of it runs
// nothing.
type controlLedger struct {
	store  *control.Store
	mu     sync.Mutex
	claims []string
}

func (l *controlLedger) ClaimDispatch(ctx context.Context, e kernel.ToolLedgerEntry) (kernel.ToolDispatchDecision, error) {
	l.mu.Lock()
	l.claims = append(l.claims, e.ToolName+"="+e.ToolCallID)
	l.mu.Unlock()
	claim, err := l.store.ClaimToolDispatch(ctx, control.DefaultTenantID, control.ToolLedgerEntry{
		RunID: e.RunID, ToolCallID: e.ToolCallID, ToolName: e.ToolName, ArgsHash: e.ArgsHash,
		RetryClass: string(e.RetryClass), EffectID: e.EffectID, PlanVersion: e.PlanVersion,
		PlanStepID: e.PlanStepID, Strategy: e.Strategy, EffectClass: e.EffectClass,
		EnvironmentGeneration: e.EnvironmentGeneration,
	})
	return kernel.ToolDispatchDecision{Execute: claim.Execute, Status: claim.Status}, err
}

func (l *controlLedger) RecordOutcome(ctx context.Context, runID, toolCallID string, ok bool) error {
	return l.store.RecordToolOutcome(ctx, control.DefaultTenantID, runID, toolCallID, ok)
}

func (l *controlLedger) claimed() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.claims...)
}

// A delegated sub-agent is one step inside the parent's run. Its streamed text
// and its own turn lifecycle must stay out of the parent's event stream, where
// the daemon would assemble them into the parent's answer; its tool activity
// and usage still belong to the parent run and stay visible, marked delegated.
// Its calls share the parent run's ledger, so a provider that sends no call
// IDs, whose calls the loop numbers from zero in each agent, must not have the
// sub-agent's calls taken for the parent's.
func TestDelegatedSubAgentEventsStayOutOfTheParentAnswer(t *testing.T) {
	provider := &scriptedStreamProvider{responses: []llm.ChatResponse{
		{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{Function: "tool_search", Args: `{"query":"delegate"}`}}},
		{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{Function: "delegate_task", Args: `{"goal":"write the report"}`}}},
		{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{Function: "read_file", Args: `{"path":"notes.txt"}`}}},
		{Content: "SUB REPORT", FinishReason: "stop"},
		{Content: "PARENT ANSWER", FinishReason: "stop"},
	}}
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ledger := &controlLedger{store: store}
	parent, read, _ := policyParent(t)
	parent.RegisterTool(tools.NewToolSearchTool())
	parent.RegisterTool(tools.NewDelegateTool())
	parentAgent := kernel.NewAgent(memory.NewMemoryManager(nil), parent, provider, "helpful", 4, 1, nil)
	cfg := &config.Config{}
	model := delegationModelSource(cfg, nil, "default", parentAgent)
	parent.InjectDelegateFn(MakeDelegateFn(parent, cfg.Delegation, nil, model))
	events := make(chan string, 4096)
	ctx := kernel.WithToolLedger(parentRunContext(t, "run_parent", t.TempDir()), ledger)
	answer, _, err := parentAgent.RunConversation(kernel.WithEventChannel(ctx, events), "person_parent", "cli", "delegate the report")
	if err != nil || !strings.Contains(answer, "PARENT ANSWER") || len(read.received()) != 1 {
		t.Fatalf("answer=%q err=%v sub-agent reads=%v ledger claims=%q", answer, err, read.received(), ledger.claimed())
	}
	var streamed strings.Builder
	turns, delegatedUsage := 0, 0
	callIDs := map[string]string{}
	for len(events) > 0 {
		event, ok := kernel.DecodeAgentEvent(<-events)
		if !ok {
			continue
		}
		delegated, _ := event.Payload["delegated"].(bool)
		switch event.Type {
		case "stream":
			streamed.WriteString(event.Content)
		case "turn.completed":
			turns++
		case "tool.started":
			if delegated != (event.ToolName == "read_file") {
				t.Fatalf("tool %s reached the parent stream with delegated=%v", event.ToolName, delegated)
			}
			if other, seen := callIDs[event.ToolCallID]; seen {
				t.Fatalf("%s and %s share call ID %q in the parent run", other, event.ToolName, event.ToolCallID)
			}
			callIDs[event.ToolCallID] = event.ToolName
		case "provider.call.usage":
			if delegated {
				delegatedUsage++
			}
		}
	}
	if strings.Contains(streamed.String(), "SUB REPORT") || turns != 1 {
		t.Fatalf("sub-agent text or turn events reached the parent stream: streamed=%q turns=%d", streamed.String(), turns)
	}
	if len(callIDs) != 3 || delegatedUsage == 0 {
		t.Fatalf("sub-agent activity lost or unmarked: tools=%v usage=%d", callIDs, delegatedUsage)
	}
	uncertain, err := store.ListUncertainToolEntries(context.Background(), control.DefaultTenantID, "run_parent", 10)
	if err != nil || len(uncertain) != 0 || len(ledger.claimed()) != 3 {
		t.Fatalf("ledger claims=%q uncertain=%+v err=%v, want three closed claims", ledger.claimed(), uncertain, err)
	}
}

// unansweredApproval is a run's approval handler when nobody answers: every
// ask times out. It records whether each ask came from a delegated sub-agent.
type unansweredApproval struct {
	mu        sync.Mutex
	delegated []bool
}

func (a *unansweredApproval) ask(ctx context.Context, _ tools.ToolApprovalRequest) (tools.ToolApprovalDecision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.delegated = append(a.delegated, kernel.DelegationNamespace(ctx) != "")
	return tools.ToolApprovalDecision{Outcome: tools.ApprovalOutcomeTimedOut, Reason: "nobody answered"}, nil
}

func (a *unansweredApproval) asks() []bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]bool(nil), a.delegated...)
}

// A sub-agent left waiting on the person, here on an approval nobody answered,
// parks the parent run on the same wait. The parent once read the wait's
// notice as the sub-agent's report and went on to answer as if it had one.
func TestSubAgentHumanWaitParksTheParentRun(t *testing.T) {
	provider := &scriptedStreamProvider{responses: []llm.ChatResponse{
		{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{Function: "tool_search", Args: `{"query":"delegate"}`}}},
		{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{Function: "delegate_task", Args: `{"goal":"fetch the release notes"}`}}},
		{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{Function: "terminal", Args: `{"command":"curl https://example.com/notes"}`}}},
		{Content: "SUB REPORT", FinishReason: "stop"},
		{Content: "PARENT ANSWER", FinishReason: "stop"},
	}}
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	approval := &unansweredApproval{}
	parent, _, terminal := policyParent(t)
	parent.RegisterTool(tools.NewToolSearchTool())
	parent.RegisterTool(tools.NewDelegateTool())
	parentAgent := kernel.NewAgent(memory.NewMemoryManager(nil), parent, provider, "helpful", 4, 1, nil)
	cfg := &config.Config{}
	parent.InjectDelegateFn(MakeDelegateFn(parent, cfg.Delegation, nil, delegationModelSource(cfg, nil, "default", parentAgent)))
	events := make(chan string, 4096)
	ctx := kernel.WithToolLedger(parentRunContext(t, "run_parent", t.TempDir(), approval.ask), &controlLedger{store: store})
	answer, _, err := parentAgent.RunConversation(kernel.WithEventChannel(ctx, events), "person_parent", "cli", "get the release notes")
	if err != nil || strings.Contains(answer, "PARENT ANSWER") || strings.Contains(answer, "SUB REPORT") {
		t.Fatalf("answer=%q err=%v, want the run parked on the sub-agent's approval", answer, err)
	}
	if got := approval.asks(); len(got) != 1 || !got[0] {
		t.Fatalf("approval asks (delegated?) = %v, want one ask from the sub-agent", got)
	}
	if got := terminal.received(); len(got) != 0 {
		t.Fatalf("an unapproved command ran: %q", got)
	}
	parked := false
	for len(events) > 0 {
		event, ok := kernel.DecodeAgentEvent(<-events)
		if !ok || event.Type != "run.outcome" {
			continue
		}
		if delegated, _ := event.Payload["delegated"].(bool); delegated {
			t.Fatalf("the sub-agent's own outcome reached the parent stream: %v", event.Payload)
		}
		needApproval, _ := event.Payload["need_approve"].(bool)
		parked = event.Payload["status"] == "waiting_user" && needApproval
	}
	if !parked {
		t.Fatal("the parent run did not park waiting on the approval")
	}
}

// goalRoutedProvider answers each delegated goal from its own script, so
// concurrent sub-agents cannot take one another's turns.
type goalRoutedProvider struct {
	goals map[string]*scriptedStreamProvider
}

func (p *goalRoutedProvider) route(req llm.ChatRequest) *scriptedStreamProvider {
	for _, message := range req.Messages {
		for goal, script := range p.goals {
			if strings.Contains(message.Content, "<delegated-goal>\n"+goal+"\n") {
				return script
			}
		}
	}
	return &scriptedStreamProvider{}
}

func (p *goalRoutedProvider) ChatCompletion(context.Context, []llm.Message) (string, error) {
	return "Done.", nil
}
func (p *goalRoutedProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	return p.route(req).Chat(ctx, req)
}
func (p *goalRoutedProvider) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	return p.route(req).StreamChat(ctx, req)
}

// When one goal of a batch waits on the person, the batch parks the parent on
// that wait, and the parent still reads what the other goals reported.
func TestBatchDelegationParksOnAGoalsHumanWait(t *testing.T) {
	provider := &goalRoutedProvider{goals: map[string]*scriptedStreamProvider{
		"summarise the notes": {responses: []llm.ChatResponse{{Content: "NOTES SUMMARY", FinishReason: "stop"}}},
		"fetch the release notes": {responses: []llm.ChatResponse{
			{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{Function: "terminal", Args: `{"command":"curl https://example.com/notes"}`}}},
		}},
	}}
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	approval := &unansweredApproval{}
	parent, _, terminal := policyParent(t)
	parentAgent := kernel.NewAgent(nil, parent, provider, "helpful", 4, 1, nil)
	cfg := &config.Config{}
	batch := MakeDelegateBatchFn(parent, cfg.Delegation, nil, delegationModelSource(cfg, nil, "default", parentAgent))
	ctx := kernel.WithToolLedger(parentRunContext(t, "run_parent", t.TempDir(), approval.ask), &controlLedger{store: store})
	results, err := batch(kernel.WithDelegationNamespace(ctx, "call_batch"), []tools.DelegateTaskSpec{
		{Goal: "summarise the notes"}, {Goal: "fetch the release notes"},
	})
	var boundary interface{ ToolRunPause() (string, string, bool) }
	if !errors.As(err, &boundary) {
		t.Fatalf("err=%v results=%+v, want the batch to park on the waiting goal", err, results)
	}
	if _, _, needApproval := boundary.ToolRunPause(); !needApproval || !strings.Contains(err.Error(), "NOTES SUMMARY") {
		t.Fatalf("pause needApproval=%v text=%q, want the approval wait and the finished goal's report", needApproval, err.Error())
	}
	if len(results) != 2 || results[0].Response != "NOTES SUMMARY" || len(terminal.received()) != 0 {
		t.Fatalf("results=%+v terminal=%q", results, terminal.received())
	}
}
