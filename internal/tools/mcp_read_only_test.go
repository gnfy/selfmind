package tools

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/tools/mcptest"
)

// connectStubServer starts the stdio stub through the real shared manager,
// once for two worker dispatchers, as the daemon does.
func connectStubServer(t *testing.T, marks MCPReadOnlyMarks, configs ...MCPServerConfig) (*MCPToolManager, []*Dispatcher) {
	t.Helper()
	return connectBuiltStubServer(t, mcptest.Build(t), marks, configs...)
}

func connectBuiltStubServer(t *testing.T, stub string, marks MCPReadOnlyMarks, configs ...MCPServerConfig) (*MCPToolManager, []*Dispatcher) {
	t.Helper()
	primary := NewDispatcherWithRegistry(NewRegistry())
	worker := NewDispatcherWithRegistry(NewRegistry())
	manager := NewMCPToolManager(primary, worker)
	if marks != nil {
		manager.UseReadOnlyMarks(marks)
	}
	t.Cleanup(func() { _ = manager.Close() })
	for _, config := range configs {
		config.Transport, config.Command = "stdio", stub
		if err := manager.Connect(config); err != nil {
			t.Fatal(err)
		}
	}
	return manager, []*Dispatcher{primary, worker}
}

// A marked tool is approved like any read-only tool in every mode: nobody is
// asked and no judge is consulted. A tool that only claims readOnlyHint is
// still an unclassified external effect: one once-only human decision, with
// its self-description kept away from the judge.
func TestOperatorReadOnlyMarkingIsApprovedLikeAReadOnlyToolAndAnnotationsAreNot(t *testing.T) {
	_, dispatchers := connectStubServer(t, nil, MCPServerConfig{Name: "stub", ReadOnlyTools: []string{mcptest.EchoTool}})
	echo := MCPToolLocalName("stub", mcptest.EchoTool)
	record := MCPToolLocalName("stub", mcptest.RecordTool)
	for _, dispatcher := range dispatchers {
		dispatcher.InjectMiddleware(SmartApprovalMiddleware(""))
	}
	modes := []ApprovalMode{ApprovalOnRequest, ApprovalReadOnly, ApprovalAutoEdit, ApprovalSmart, ApprovalFullAuto}
	for index, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			person := fmt.Sprintf("person-mcp-read-only-%d", index)
			var asks []ToolApprovalRequest
			judge := &fakeJudge{reply: "APPROVE"}
			cleanup := SetExecutionScope(person, ExecutionScope{TenantID: "default", PersonID: person, RunID: "run-" + person,
				ApprovalMode: mode, Judge: judge,
				Approval: func(_ context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
					asks = append(asks, req)
					return ToolApprovalDecision{Approved: true, ApprovalID: "apr-" + person}, nil
				}})
			defer cleanup()
			dispatcher := dispatchers[index%2]
			if out, err := dispatcher.Dispatch(echo, map[string]interface{}{"_tenant_id": person, "text": "marked"}); err != nil || out != "marked" {
				t.Fatalf("marked tool = %q, %v", out, err)
			}
			if len(asks) != 0 || judge.calls != 0 {
				t.Fatalf("marked read-only tool asked %d time(s), judged %d time(s)", len(asks), judge.calls)
			}
			if out, err := dispatcher.Dispatch(record, map[string]interface{}{"_tenant_id": person, "text": "change"}); err != nil || !strings.Contains(out, "recorded") {
				t.Fatalf("annotated writing tool = %q, %v", out, err)
			}
			if len(asks) != 1 || asks[0].DecisionPolicy != ApprovalDecisionPolicyOnceOnly || asks[0].Reason != unclassifiedExternalApprovalReason {
				t.Fatalf("readOnlyHint was trusted: asks=%+v", asks)
			}
			if judge.calls != 0 {
				t.Fatalf("the server's self-description reached smart triage %d time(s)", judge.calls)
			}
		})
	}
}

// A marked tool reads through the network, so the person's explicit "do not
// use the network" still reaches it: even full-auto asks. A prohibition of
// something it does not do, such as modifying files, leaves it alone.
func TestExplicitDenyStillReachesAMarkedMCPTool(t *testing.T) {
	_, dispatchers := connectStubServer(t, nil, MCPServerConfig{Name: "deny", ReadOnlyTools: []string{mcptest.EchoTool}})
	echo := MCPToolLocalName("deny", mcptest.EchoTool)
	dispatcher := dispatchers[0]
	dispatcher.InjectMiddleware(SmartApprovalMiddleware(""))
	for name, tc := range map[string]struct {
		deny DenyScope
		asks int
	}{
		"network deny": {DenyScope{Marker: "do not use the network", Clause: "do not use the network",
			Classes: []OperationClass{OpClassNetwork}, Resolved: true}, 1},
		"write deny": {DenyScope{Marker: "do not modify files", Clause: "do not modify files",
			Classes: []OperationClass{OpClassWrite}, Resolved: true}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			person := "person-deny-" + strings.ReplaceAll(name, " ", "-")
			var asks []ToolApprovalRequest
			cleanup := SetExecutionScope(person, ExecutionScope{TenantID: "default", PersonID: person, RunID: "run-" + person,
				ApprovalMode: ApprovalFullAuto,
				IntentSnapshot: func() RunIntentSnapshot {
					return RunIntentSnapshot{ExplicitDeny: []string{tc.deny.Marker}, DenyScopes: []DenyScope{tc.deny}}
				},
				Approval: func(_ context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
					asks = append(asks, req)
					return ToolApprovalDecision{Approved: true, ApprovalID: "apr-" + person}, nil
				}})
			defer cleanup()
			if out, err := dispatcher.Dispatch(echo, map[string]interface{}{"_tenant_id": person, "text": "look"}); err != nil || out != "look" {
				t.Fatalf("marked tool = %q, %v", out, err)
			}
			if len(asks) != tc.asks {
				t.Fatalf("asks = %d, want %d (%+v)", len(asks), tc.asks, asks)
			}
		})
	}
	metadata := dispatcher.ToolExecutionMetadata(echo, map[string]interface{}{})
	if !metadata.ReadOnly || !slices.Contains(metadata.OperationClasses, string(OpClassNetwork)) ||
		slices.Contains(metadata.OperationClasses, string(OpClassWrite)) {
		t.Fatalf("marked tool metadata = %+v, want a read-only network read", metadata)
	}
}

// claimProbe sits inside the effect-claim middleware and records, while a
// call is dispatched, which unresolved targets the person holds. The view is
// taken in flight, so it does not depend on when a claim is later released.
type claimProbe struct {
	mu      sync.Mutex
	store   *control.Store
	tenant  string
	person  string
	targets map[string][]string
}

func (p *claimProbe) middleware(next ResultExecutor) ResultExecutor {
	return func(args map[string]interface{}) (kernel.ToolDispatchResult, error) {
		claims, err := p.store.ListUnresolvedExternalEffects(context.Background(), p.tenant, p.person, 20)
		var held []string
		for _, claim := range claims {
			held = append(held, claim.TargetKey)
		}
		p.mu.Lock()
		p.targets[stringArg(args, "_tool_call_id")] = append(held, fmt.Sprintf("err=%v", err))
		p.mu.Unlock()
		return next(args)
	}
}

func (p *claimProbe) seen(call string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.targets[call]
}

// At parallel capacity a marked tool claims no external target, so two Runs
// of one person can both call it; the tool that only claims readOnlyHint still
// takes the person-wide unknown target.
func TestMarkedMCPToolClaimsNoTargetWhileAnnotatedToolClaimsUnknown(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "owner", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	_, dispatchers := connectStubServer(t, store, MCPServerConfig{Name: "claims", ReadOnlyTools: []string{mcptest.RendezvousTool}})
	probe := &claimProbe{store: store, tenant: owner.TenantID, person: owner.PersonID, targets: map[string][]string{}}
	var runs []*control.Run
	for index, dispatcher := range dispatchers {
		dispatcher.InjectResultMiddleware(ExternalEffectClaimMiddleware(store))
		dispatcher.InjectResultMiddleware(probe.middleware)
		task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID,
			Title: fmt.Sprint("run ", index), Channel: fmt.Sprint("session-", index)})
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.StartRunWithOptions(ctx, task, task.Title, task.Title, control.StartRunOptions{MaxActiveRuns: 2})
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, run)
		t.Cleanup(SetExecutionScope(ExecutionScopeKeyForRun(run.ID), ExecutionScope{TenantID: owner.TenantID,
			PersonID: owner.PersonID, RunID: run.ID, ParallelWork: true}))
	}
	dispatch := func(index int, tool, call string, args map[string]interface{}) (string, error) {
		args["_tool_call_id"] = call
		args["_invocation_scope"] = kernel.ToolInvocationScope{ExecutionScopeKey: ExecutionScopeKeyForRun(runs[index].ID)}
		return dispatchers[index].Dispatch(tool, args)
	}

	rendezvous := MCPToolLocalName("claims", mcptest.RendezvousTool)
	var wg sync.WaitGroup
	results := make([]string, 2)
	errs := make([]error, 2)
	for index := range runs {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			results[index], errs[index] = dispatch(index, rendezvous, fmt.Sprint("meet-", index),
				map[string]interface{}{"key": "marked", "parties": 2, "timeout_ms": 10000})
		}(index)
	}
	wg.Wait()
	for index := range runs {
		if errs[index] != nil || !strings.Contains(results[index], "2 parties met") {
			t.Fatalf("run %d marked call = %q, err=%v", index, results[index], errs[index])
		}
		if held := probe.seen(fmt.Sprint("meet-", index)); len(held) != 1 || held[0] != "err=<nil>" {
			t.Fatalf("run %d held external targets while calling a marked tool: %v", index, held)
		}
	}

	record := MCPToolLocalName("claims", mcptest.RecordTool)
	if _, err := dispatch(0, record, "write-0", map[string]interface{}{"text": "change"}); err != nil {
		t.Fatalf("annotated writing tool: %v", err)
	}
	if held := probe.seen("write-0"); len(held) != 2 || held[0] != control.UnknownExternalTarget {
		t.Fatalf("readOnlyHint narrowed the claim: in-flight targets %v", held)
	}
}

// The kernel batches calls by this answer and records the policy it reports.
func TestMarkedMCPToolJoinsReadOnlyBatchesAndReportsReadOnlyMetadata(t *testing.T) {
	_, dispatchers := connectStubServer(t, nil, MCPServerConfig{Name: "batch", ReadOnlyTools: []string{mcptest.EchoTool}})
	echo := MCPToolLocalName("batch", mcptest.EchoTool)
	record := MCPToolLocalName("batch", mcptest.RecordTool)
	args := map[string]interface{}{"_tenant_id": "person-batch"}
	for index, dispatcher := range dispatchers {
		if !dispatcher.SupportsParallelToolCall(echo, args) {
			t.Fatalf("dispatcher %d: marked tool cannot join a read-only batch", index)
		}
		if dispatcher.SupportsParallelToolCall(record, args) || dispatcher.SupportsParallelTool(record) {
			t.Fatalf("dispatcher %d: annotated writing tool joined a read-only batch", index)
		}
		marked := dispatcher.ToolExecutionMetadata(echo, args)
		if marked.Origin != string(ToolSchemaOriginExternal) || !marked.ReadOnly || !marked.ObservationOnly {
			t.Fatalf("dispatcher %d: marked tool metadata = %+v", index, marked)
		}
		if retry := kernel.ClassifyToolRetry(echo, marked); retry != kernel.ToolRetrySideEffect {
			t.Fatalf("a marking granted blind replay: retry class %s", retry)
		}
		annotated := dispatcher.ToolExecutionMetadata(record, args)
		if annotated.ReadOnly || annotated.ObservationOnly {
			t.Fatalf("dispatcher %d: annotated tool metadata = %+v", index, annotated)
		}
	}
}

// A person's marking covers only that person, that exact tool, and the server
// identity it was made for. The same name configured with another command is
// another server and inherits nothing.
func TestPersonReadOnlyMarkingIsPersonalAndBoundToServerIdentity(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "owner", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.ResolveOrCreateAccount(ctx, "default", "telegram", "someone-else", "Other")
	if err != nil {
		t.Fatal(err)
	}
	stub := mcptest.Build(t)
	manager, dispatchers := connectBuiltStubServer(t, stub, store, MCPServerConfig{Name: "personal"})
	status := manager.Servers()
	if len(status) != 1 || status[0].Name != "personal" || status[0].Identity == "" || !status[0].Connected {
		t.Fatalf("server status = %+v", status)
	}
	if _, _, err := store.MarkMCPToolReadOnly(ctx, control.MCPReadOnlyMarkRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
		ServerName: "personal", ServerIdentity: status[0].Identity, ToolName: mcptest.EchoTool,
		ActorAccountID: owner.AccountID, ActorChannel: "session-1"}); err != nil {
		t.Fatal(err)
	}
	echo := MCPToolLocalName("personal", mcptest.EchoTool)
	rendezvous := MCPToolLocalName("personal", mcptest.RendezvousTool)
	policyFor := func(dispatcher *Dispatcher, person, tool string) toolExecutionPolicy {
		t.Helper()
		key := "policy-" + person
		cleanup := SetExecutionScope(key, ExecutionScope{TenantID: "default", PersonID: person,
			StandingGrants: InteractiveStandingGrants()})
		defer cleanup()
		registered, ok := dispatcher.GetTool(tool)
		if !ok {
			t.Fatalf("%s is not registered", tool)
		}
		return executionPolicyForCall(registered, map[string]interface{}{"_tenant_id": key})
	}
	for index, dispatcher := range dispatchers {
		if policy := policyFor(dispatcher, owner.PersonID, echo); policy.ReadOnlyMarkedBy != ReadOnlyMarkedByPerson || !policy.ReadOnly {
			t.Fatalf("dispatcher %d: the marking person's policy = %+v", index, policy)
		}
		if policy := policyFor(dispatcher, other.PersonID, echo); policy.ReadOnlyMarkedBy != "" || policy.ReadOnly {
			t.Fatalf("dispatcher %d: another person inherited the marking: %+v", index, policy)
		}
		if policy := policyFor(dispatcher, owner.PersonID, rendezvous); policy.ReadOnlyMarkedBy != "" {
			t.Fatalf("dispatcher %d: the marking spread to another tool: %+v", index, policy)
		}
	}

	// A daemon restarted with only the arguments of this server changed.
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	replaced, replacedDispatchers := connectBuiltStubServer(t, stub, store, MCPServerConfig{Name: "personal",
		Args: mcptest.StartLogArgs(t.TempDir() + "/starts.log")})
	if got := replaced.Servers()[0].Identity; got == status[0].Identity {
		t.Fatalf("changed arguments kept the server identity %s", got)
	}
	for index, dispatcher := range replacedDispatchers {
		if policy := policyFor(dispatcher, owner.PersonID, echo); policy.ReadOnlyMarkedBy != "" || policy.ReadOnly {
			t.Fatalf("dispatcher %d: a differently configured server inherited the marking: %+v", index, policy)
		}
	}
	// The unchanged configuration is the same server again.
	if err := replaced.Close(); err != nil {
		t.Fatal(err)
	}
	restored, restoredDispatchers := connectBuiltStubServer(t, stub, store, MCPServerConfig{Name: "personal"})
	if restored.Servers()[0].Identity != status[0].Identity {
		t.Fatal("an unchanged configuration got another identity")
	}
	if policy := policyFor(restoredDispatchers[1], owner.PersonID, echo); policy.ReadOnlyMarkedBy != ReadOnlyMarkedByPerson {
		t.Fatalf("the original server lost its marking: %+v", policy)
	}
}

// A person's marking is a standing decision, so a scheduled Run carries only
// the markings that existed when its schedule was authorised, and a Run
// admitted without standing decisions uses none. The operator's marking is
// configuration and applies to every Run.
func TestPersonReadOnlyMarkingFollowsTheRunsStandingDecisions(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "owner", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	manager, dispatchers := connectStubServer(t, store, MCPServerConfig{Name: "scheduled", ReadOnlyTools: []string{mcptest.RendezvousTool}})
	mark, _, err := store.MarkMCPToolReadOnly(ctx, control.MCPReadOnlyMarkRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
		ServerName: "scheduled", ServerIdentity: manager.Servers()[0].Identity, ToolName: mcptest.EchoTool,
		ActorAccountID: owner.AccountID, ActorChannel: "session-1"})
	if err != nil {
		t.Fatal(err)
	}
	echo, _ := dispatchers[0].GetTool(MCPToolLocalName("scheduled", mcptest.EchoTool))
	rendezvous, _ := dispatchers[0].GetTool(MCPToolLocalName("scheduled", mcptest.RendezvousTool))
	for name, tc := range map[string]struct {
		grants   StandingGrantPolicy
		personal string
	}{
		"interactive":                        {InteractiveStandingGrants(), ReadOnlyMarkedByPerson},
		"scheduled after the marking":        {ScheduledStandingGrants(mark.MarkedAt.Add(time.Minute)), ReadOnlyMarkedByPerson},
		"scheduled before the marking":       {ScheduledStandingGrants(mark.MarkedAt.Add(-time.Minute)), ""},
		"scheduled without an authorisation": {ScheduledStandingGrants(time.Time{}), ""},
	} {
		key := "standing-" + strings.ReplaceAll(name, " ", "-")
		cleanup := SetExecutionScope(key, ExecutionScope{TenantID: owner.TenantID, PersonID: owner.PersonID, StandingGrants: tc.grants})
		args := map[string]interface{}{"_tenant_id": key}
		if got := executionPolicyForCall(echo, args).ReadOnlyMarkedBy; got != tc.personal {
			t.Errorf("%s: person marking = %q, want %q", name, got, tc.personal)
		}
		if got := executionPolicyForCall(rendezvous, args).ReadOnlyMarkedBy; got != ReadOnlyMarkedByOperator {
			t.Errorf("%s: operator marking = %q", name, got)
		}
		cleanup()
	}
}

// withdrawnAfterFirstLookup answers "marked" to its first lookup only, as if
// the person unmarked the tool right after a batch was planned.
type withdrawnAfterFirstLookup struct {
	mu      sync.Mutex
	lookups int
}

func (m *withdrawnAfterFirstLookup) MCPToolMarkedReadOnly(context.Context, string, string, string, string, string, time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lookups++
	return m.lookups == 1, nil
}

func (m *withdrawnAfterFirstLookup) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lookups
}

// One call reads its marking once and carries the answer through batching,
// metadata and dispatch, so an unmark that lands mid-call cannot give one call
// two policies. The next call reads again and sees the withdrawal.
func TestReadOnlyMarkingIsReadOnceAndCarriedThroughTheCall(t *testing.T) {
	marks := &withdrawnAfterFirstLookup{}
	_, dispatchers := connectStubServer(t, marks, MCPServerConfig{Name: "once"})
	echo := MCPToolLocalName("once", mcptest.EchoTool)
	dispatcher := dispatchers[0]
	dispatcher.InjectMiddleware(SmartApprovalMiddleware(""))
	var asks int
	cleanup := SetExecutionScope("person-once", ExecutionScope{TenantID: "default", PersonID: "person-once", RunID: "run-once",
		ApprovalMode: ApprovalOnRequest, StandingGrants: InteractiveStandingGrants(),
		Approval: func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error) {
			asks++
			return ToolApprovalDecision{Approved: true, ApprovalID: "apr-once"}, nil
		}})
	defer cleanup()

	call := map[string]interface{}{"_tenant_id": "person-once"}
	if !dispatcher.SupportsParallelToolCall(echo, call) {
		t.Fatal("the first read did not see the marking")
	}
	call["text"] = "first"
	if metadata := dispatcher.ToolExecutionMetadata(echo, call); !metadata.ReadOnly {
		t.Fatalf("the call's metadata re-read the marking: %+v", metadata)
	}
	if out, err := dispatcher.Dispatch(echo, call); err != nil || out != "first" || asks != 0 {
		t.Fatalf("the call's dispatch re-read the marking: out=%q err=%v asks=%d", out, err, asks)
	}
	if got := marks.count(); got != 1 {
		t.Fatalf("one call read its marking %d times", got)
	}

	next := map[string]interface{}{"_tenant_id": "person-once", "text": "second"}
	if out, err := dispatcher.Dispatch(echo, next); err != nil || out != "second" || asks != 1 {
		t.Fatalf("the next call did not see the withdrawal: out=%q err=%v asks=%d", out, err, asks)
	}
	if got := marks.count(); got != 2 {
		t.Fatalf("lookups after two calls = %d", got)
	}
}

// The identity is what the configuration starts or reaches, not its name or
// credentials, and the HTTP transport aliases are one transport.
func TestMCPServerIdentityFollowsTransportCommandArgsAndURL(t *testing.T) {
	base := MCPServerIdentity(MCPServerConfig{Name: "a", Transport: "stdio", Command: "server", Args: []string{"--read"}})
	for name, config := range map[string]MCPServerConfig{
		"another command":   {Name: "a", Transport: "stdio", Command: "server2", Args: []string{"--read"}},
		"another argument":  {Name: "a", Transport: "stdio", Command: "server", Args: []string{"--write"}},
		"an extra argument": {Name: "a", Transport: "stdio", Command: "server", Args: []string{"--read", "--all"}},
		"split argument":    {Name: "a", Transport: "stdio", Command: "server --read"},
		"http transport":    {Name: "a", Transport: "http", URL: "server"},
	} {
		if MCPServerIdentity(config) == base {
			t.Errorf("%s kept the identity", name)
		}
	}
	for name, config := range map[string]MCPServerConfig{
		"another name":                   {Name: "b", Transport: "stdio", Command: "server", Args: []string{"--read"}},
		"transport case and env filter":  {Name: "a", Transport: "STDIO", Command: "server", Args: []string{"--read"}, EnvFilter: []string{"HOME"}},
		"operator read-only tool list":   {Name: "a", Transport: "stdio", Command: "server", Args: []string{"--read"}, ReadOnlyTools: []string{"x"}},
		"surrounding command whitespace": {Name: "a", Transport: "stdio", Command: " server ", Args: []string{"--read"}},
	} {
		if MCPServerIdentity(config) != base {
			t.Errorf("%s changed the identity", name)
		}
	}
	remote := MCPServerIdentity(MCPServerConfig{Transport: "http", URL: "https://mcp.example/a", Headers: map[string]string{"Authorization": "x"}})
	for _, transport := range []string{"streamable_http", "streamable-http"} {
		if MCPServerIdentity(MCPServerConfig{Transport: transport, URL: "https://mcp.example/a", Auth: map[string]string{"bearer": "y"}}) != remote {
			t.Fatalf("transport alias %s or a credential changed the identity", transport)
		}
	}
	if MCPServerIdentity(MCPServerConfig{Transport: "http", URL: "https://mcp.example/b"}) == remote {
		t.Fatal("another URL kept the identity")
	}
}

// An operator name the server does not expose is a diagnostic, not a failure:
// the server stays connected and the names it does expose stay marked.
func TestOperatorReadOnlyNamesTheServerDoesNotExposeAreWarnings(t *testing.T) {
	manager, dispatchers := connectStubServer(t, nil, MCPServerConfig{Name: "warned",
		ReadOnlyTools: []string{mcptest.EchoTool, "search_code"}})
	health := manager.Health()
	if health.Connected != 1 || health.Failed != 0 || len(health.Failures) != 0 {
		t.Fatalf("an unknown read-only name failed the server: %+v", health)
	}
	if len(health.Warnings) != 1 || health.Warnings[0].Name != "warned" ||
		!strings.Contains(health.Warnings[0].Warning, "search_code") || strings.Contains(health.Warnings[0].Warning, mcptest.EchoTool) {
		t.Fatalf("warnings = %+v", health.Warnings)
	}
	echo, _ := dispatchers[1].GetTool(MCPToolLocalName("warned", mcptest.EchoTool))
	if policy := executionPolicyForCall(echo, map[string]interface{}{}); policy.ReadOnlyMarkedBy != ReadOnlyMarkedByOperator {
		t.Fatalf("exposed operator name lost its marking: %+v", policy)
	}
}
