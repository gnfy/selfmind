package kernel

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"selfmind/internal/kernel/llm"
)

// runScopedParallelBackend allows a batch only for one person's Run, the way
// a read-only marking applies to the person who made it. Its calls meet at a
// barrier: a call returns once both are in flight, or fails when its partner
// never arrives, so a serialized batch cannot pass for a parallel one.
type runScopedParallelBackend struct {
	person   string
	parties  int
	mu       sync.Mutex
	arrived  int
	met      chan struct{}
	asked    []map[string]interface{}
	inFlight int
	peak     int
}

func newRunScopedParallelBackend(person string, parties int) *runScopedParallelBackend {
	return &runScopedParallelBackend{person: person, parties: parties, met: make(chan struct{})}
}

func (*runScopedParallelBackend) GetToolDefinitions() []map[string]interface{} { return nil }

// The name-only answer knows no Run, so it never allows the external tool.
func (*runScopedParallelBackend) SupportsParallelTool(string) bool { return false }

func (b *runScopedParallelBackend) SupportsParallelToolCall(_ string, args map[string]interface{}) bool {
	b.mu.Lock()
	b.asked = append(b.asked, args)
	b.mu.Unlock()
	scope, _ := args["_invocation_scope"].(ToolInvocationScope)
	return scope.PersonID == b.person
}

func (b *runScopedParallelBackend) Dispatch(string, map[string]interface{}) (string, error) {
	b.mu.Lock()
	b.inFlight++
	b.peak = max(b.peak, b.inFlight)
	b.arrived++
	if b.arrived == b.parties {
		close(b.met)
	}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.inFlight--
		b.mu.Unlock()
	}()
	if b.parties < 2 {
		time.Sleep(20 * time.Millisecond)
		return "looked", nil
	}
	select {
	case <-b.met:
		return "looked", nil
	case <-time.After(5 * time.Second):
		return "", fmt.Errorf("the other call of the batch never ran alongside this one")
	}
}

// batchLedger lets every call of a batch claim its dispatch concurrently.
type batchLedger struct {
	mu      sync.Mutex
	claimed int
}

func (l *batchLedger) ClaimDispatch(context.Context, ToolLedgerEntry) (ToolDispatchDecision, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.claimed++
	return ToolDispatchDecision{Execute: true, Status: "started"}, nil
}

func (*batchLedger) RecordOutcome(context.Context, string, string, bool) error { return nil }

func runContext(person, run string) context.Context {
	ctx := WithToolInvocationScope(context.Background(), ToolInvocationScope{
		ControlTenantID: "default", PersonID: person, RunID: run})
	return WithToolLedger(ctx, &batchLedger{})
}

func lookupCalls() []llm.ToolCall {
	return []llm.ToolCall{
		{ID: "look-1", Function: "mcp_tracker_lookup_1a2b3c4d", Args: `{"id":"1"}`},
		{ID: "look-2", Function: "mcp_tracker_lookup_1a2b3c4d", Args: `{"id":"2"}`},
	}
}

// A batch of calls that the Run's own policy allows runs in parallel; the
// backend answers with the same trusted runtime arguments each call carries.
func TestBatchParallelismIsDecidedWithTheRunScope(t *testing.T) {
	backend := newRunScopedParallelBackend("person-marked", 2)
	agent := &Agent{backend: backend}
	results := agent.executeToolCalls(runContext("person-marked", "run-marked"), "tenant-key", nil, lookupCalls())
	for _, result := range results {
		if !result.success {
			t.Fatalf("a batch the Run's policy allows did not run in parallel: %+v", result.msg)
		}
	}
	if len(backend.asked) != 2 {
		t.Fatalf("parallel support asked %d time(s), want once per call", len(backend.asked))
	}
	for _, args := range backend.asked {
		scope, _ := args["_invocation_scope"].(ToolInvocationScope)
		if args["_tenant_id"] != "tenant-key" || args["_context"] == nil || scope.RunID != "run-marked" {
			t.Fatalf("parallel support was asked without the call's runtime scope: %+v", args)
		}
	}
}

// carriedDecisionBackend records a per-call fact while deciding the batch and
// reports what each dispatched call carried.
type carriedDecisionBackend struct {
	mu      sync.Mutex
	carried []interface{}
}

func (*carriedDecisionBackend) GetToolDefinitions() []map[string]interface{} { return nil }
func (*carriedDecisionBackend) SupportsParallelTool(string) bool             { return false }

func (b *carriedDecisionBackend) SupportsParallelToolCall(name string, args map[string]interface{}) bool {
	args["_decided_for"] = "decided:" + name
	return true
}

func (b *carriedDecisionBackend) Dispatch(_ string, args map[string]interface{}) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.carried = append(b.carried, args["_decided_for"])
	return "looked", nil
}

// A fact the backend recorded while deciding the batch travels with that call
// into dispatch, so the call is executed under the answer it was batched by.
// The model cannot supply such a fact itself.
func TestBatchDecisionTravelsWithItsCall(t *testing.T) {
	backend := &carriedDecisionBackend{}
	agent := &Agent{backend: backend}
	calls := lookupCalls()
	calls[1].Args = `{"id":"2","_decided_for":"forged"}`
	for _, result := range agent.executeToolCalls(runContext("person-marked", "run-marked"), "tenant-key", nil, calls) {
		if !result.success {
			t.Fatalf("call failed: %+v", result.msg)
		}
	}
	if len(backend.carried) != 2 {
		t.Fatalf("dispatched %d calls", len(backend.carried))
	}
	for _, fact := range backend.carried {
		if fact != "decided:mcp_tracker_lookup_1a2b3c4d" {
			t.Fatalf("a call did not carry its batch decision: %v", backend.carried)
		}
	}
}

// The same calls in another person's Run stay sequential.
func TestBatchParallelismStaysSequentialForAnotherRun(t *testing.T) {
	backend := newRunScopedParallelBackend("person-marked", 0)
	agent := &Agent{backend: backend}
	results := agent.executeToolCalls(runContext("person-other", "run-other"), "tenant-key", nil, lookupCalls())
	for _, result := range results {
		if !result.success {
			t.Fatalf("sequential call failed: %+v", result.msg)
		}
	}
	if backend.peak != 1 {
		t.Fatalf("another person's Run ran %d calls at once", backend.peak)
	}
}
