package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/httpapi"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/tools"
	"selfmind/internal/tools/mcptest"
)

// heldTargetsProbe runs inside each worker's effect-claim middleware and
// records, while an MCP call is dispatched, which unresolved external targets
// the person holds. The view is taken in flight, so it does not depend on
// when a claim is later released.
type heldTargetsProbe struct {
	store  *control.Store
	tenant string
	person string
	mu     sync.Mutex
	seen   map[string][]string
}

func (p *heldTargetsProbe) install(workers []AgentWorker) {
	for _, worker := range workers {
		worker.Dispatcher.InjectResultMiddleware(func(next tools.ResultExecutor) tools.ResultExecutor {
			return func(args map[string]interface{}) (kernel.ToolDispatchResult, error) {
				if name, _ := args["_tool_name"].(string); strings.HasPrefix(name, "mcp_") {
					claims, err := p.store.ListUnresolvedExternalEffects(context.Background(), p.tenant, p.person, 20)
					held := []string{}
					for _, claim := range claims {
						held = append(held, claim.TargetKey)
					}
					if err != nil {
						held = append(held, "error: "+err.Error())
					}
					call, _ := args["_tool_call_id"].(string)
					p.mu.Lock()
					p.seen[call] = held
					p.mu.Unlock()
				}
				return next(args)
			}
		})
	}
}

func (p *heldTargetsProbe) heldDuring(call string) ([]string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	held, ok := p.seen[call]
	return held, ok
}

// meetScript has each Run call one MCP tool, tagged with the Run's marker,
// and answer with the tool's result.
func meetScript(tool string, arguments func(marker string) string) scriptedModelScript {
	return func(request scriptedModelRequest) scriptedModelReply {
		marker := runMarker(request.Messages)
		if len(request.Tools) == 0 || marker == "" {
			return scriptedModelReply{Text: "ok"}
		}
		for _, message := range request.Messages {
			if message.Role == "tool" && message.ToolCallID == "call-"+marker {
				return scriptedModelReply{Text: fmt.Sprintf("Called for %s: %s", marker, message.Content)}
			}
		}
		return scriptedModelReply{ToolCalls: []llm.ToolCall{{ID: "call-" + marker, Function: tool, Args: arguments(marker)}}}
	}
}

func newParallelMCPServer(t *testing.T, h *parityHarness, deps *GatewayDeps) *httpapi.Server {
	t.Helper()
	server := &httpapi.Server{
		Control: h.store, Gateway: deps.Gateway, DefaultTenantID: control.DefaultTenantID,
		ApprovalWait: 30 * time.Second, ApprovalWaitUnattended: 30 * time.Second,
	}
	if err := server.ConfigureWorkRunCapacity(2, len(deps.Workers)); err != nil {
		t.Fatal(err)
	}
	return server
}

// At capacity 2 on two workers, two Runs of one person call the same shared
// MCP server at once. A tool marked read-only, by the operator or by the
// person, claims no external target, so both calls are in flight together
// (the rendezvous only completes then) and neither Run waits on the other.
func TestTwoRunsCallAMarkedMCPToolConcurrentlyWithoutClaims(t *testing.T) {
	for _, marking := range []string{tools.ReadOnlyMarkedByOperator, tools.ReadOnlyMarkedByPerson} {
		t.Run(marking, func(t *testing.T) {
			rendezvous := tools.MCPToolLocalName("stub", mcptest.RendezvousTool)
			h, opts := parityGatewayOptions(t, 2, "stub", meetScript(rendezvous, func(string) string {
				return `{"key":"two-runs","parties":2,"timeout_ms":20000}`
			}))
			if marking == tools.ReadOnlyMarkedByOperator {
				opts.Config.MCP.Servers[0].ReadOnlyTools = []string{mcptest.RendezvousTool}
			}
			deps, err := InitGateway(opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(deps.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			identity, err := h.store.ResolveOrCreateAccount(ctx, control.DefaultTenantID, "cli", "local", "Local")
			if err != nil {
				t.Fatal(err)
			}
			if marking == tools.ReadOnlyMarkedByPerson {
				if _, _, err := h.store.MarkMCPToolReadOnly(ctx, control.MCPReadOnlyMarkRequest{TenantID: identity.TenantID,
					PersonID: identity.PersonID, ServerName: "stub", ServerIdentity: deps.MCP.Servers()[0].Identity,
					ToolName: mcptest.RendezvousTool, ActorAccountID: identity.AccountID, ActorChannel: "cli-setup"}); err != nil {
					t.Fatal(err)
				}
			}
			probe := &heldTargetsProbe{store: h.store, tenant: identity.TenantID, person: identity.PersonID, seen: map[string][]string{}}
			probe.install(deps.Workers)
			server := newParallelMCPServer(t, h, deps)

			// Each session works in its own project, so nothing local makes
			// one Run wait for the other.
			markers := []string{"alpha", "beta"}
			workspaces := make([]string, len(markers))
			for index, marker := range markers {
				workspace, err := h.store.EnsureWorkspace(ctx, control.Workspace{TenantID: identity.TenantID,
					OwnerPersonID: identity.PersonID, Name: marker, LocalPath: t.TempDir()})
				if err != nil {
					t.Fatal(err)
				}
				workspaces[index] = workspace.ID
			}
			replies := make([]api.MessageResponse, len(markers))
			codes := make([]int, len(markers))
			var wg sync.WaitGroup
			for index, marker := range markers {
				wg.Add(1)
				go func(index int, marker string) {
					defer wg.Done()
					replies[index], codes[index] = server.ProcessMessage(ctx, api.MessageRequest{
						Platform: "cli", PlatformUserID: "local", Channel: "session-" + marker, WorkspaceID: workspaces[index],
						Content: "Look this up. parity-marker:" + marker, ApprovalMode: "on-request",
					})
				}(index, marker)
			}
			wg.Wait()
			for index, marker := range markers {
				if codes[index] != http.StatusOK || !strings.Contains(replies[index].Content, "2 parties met") {
					t.Fatalf("run %s did not call the marked tool alongside the other run: code=%d content=%q error=%q",
						marker, codes[index], replies[index].Content, replies[index].Error)
				}
				held, called := probe.heldDuring("call-" + marker)
				if !called || len(held) != 0 {
					t.Fatalf("run %s held external targets while calling a marked tool: called=%t held=%v", marker, called, held)
				}
			}
			if claims, err := h.store.ListUnresolvedExternalEffects(ctx, identity.TenantID, identity.PersonID, 20); err != nil || len(claims) != 0 {
				t.Fatalf("marked calls left external claims: %+v err=%v", claims, err)
			}
			if starts := mcptest.Starts(t, h.startLog); len(starts) != 1 {
				t.Fatalf("MCP server processes = %d, want one shared process", len(starts))
			}
		})
	}
}

// The same production path with an unmarked tool that claims readOnlyHint:
// it is still an unclassified external effect, approved once by the person
// and dispatched under the person-wide unknown target.
func TestUnmarkedMCPToolStillClaimsTheUnknownTargetAtCapacityTwo(t *testing.T) {
	record := tools.MCPToolLocalName("stub", mcptest.RecordTool)
	h, opts := parityGatewayOptions(t, 2, "stub", meetScript(record, func(marker string) string {
		return fmt.Sprintf(`{"text":"change from %s"}`, marker)
	}))
	opts.Config.MCP.Servers[0].ReadOnlyTools = []string{mcptest.EchoTool}
	deps, err := InitGateway(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(deps.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	identity, err := h.store.ResolveOrCreateAccount(ctx, control.DefaultTenantID, "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	probe := &heldTargetsProbe{store: h.store, tenant: identity.TenantID, person: identity.PersonID, seen: map[string][]string{}}
	probe.install(deps.Workers)
	server := newParallelMCPServer(t, h, deps)
	stopApprover := approvePending(ctx, t, h.store, identity)
	defer stopApprover()

	reply, code := server.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "session-gamma",
		Content: "Record this. parity-marker:gamma", ApprovalMode: "full-auto",
	})
	if code != http.StatusOK || !strings.Contains(reply.Content, "recorded") {
		t.Fatalf("unmarked call: code=%d content=%q error=%q", code, reply.Content, reply.Error)
	}
	held, called := probe.heldDuring("call-gamma")
	if !called || len(held) != 1 || held[0] != control.UnknownExternalTarget {
		t.Fatalf("an unmarked tool claiming readOnlyHint was dispatched holding %v (called=%t), want the unknown target", held, called)
	}
	approvals, err := h.store.ListApprovalRequests(ctx, identity.TenantID, identity.PersonID, "approved", 20)
	if err != nil || len(approvals) != 1 {
		t.Fatalf("unmarked call approvals = %+v err=%v, want the person's one decision", approvals, err)
	}
}
