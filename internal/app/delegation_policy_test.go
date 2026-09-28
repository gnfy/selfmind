package app

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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
}

func (t *recordingTool) Name() string        { return t.name }
func (t *recordingTool) Description() string { return "records its " + t.param }
func (t *recordingTool) Schema() tools.ToolSchema {
	return tools.ToolSchema{Type: "object", Properties: map[string]tools.PropertyDef{t.param: {Type: "string"}}, Required: []string{t.param}}
}
func (t *recordingTool) Execute(args map[string]interface{}) (string, error) {
	value, _ := args[t.param].(string)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.got = append(t.got, value)
	return "recorded", nil
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
}

func (p *scriptedStreamProvider) next() llm.ChatResponse {
	p.mu.Lock()
	defer p.mu.Unlock()
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
func (p *scriptedStreamProvider) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	resp := p.next()
	return &resp, nil
}
func (p *scriptedStreamProvider) StreamChat(context.Context, llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	resp := p.next()
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
// run's execution scope, its context key and its trusted invocation scope.
func parentRunContext(t *testing.T, runID, workspace string) context.Context {
	t.Cleanup(tools.SetExecutionScope("person_parent", tools.ExecutionScope{
		PersonID: "person_parent", RunID: runID, WorkspaceRoot: workspace, AllowedRoots: []string{workspace},
	}))
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
	mem := memory.NewMemoryManager(nil)
	sub := buildDelegateSubBackend(mem, parent, config.DelegationConfig{}, nil, nil, 1)
	if _, _, err := runDelegatedGoal(parentRunContext(t, "run_parent", workspace), mem, sub, provider, nil, 4, 1, "inspect the notes"); err != nil {
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
			return buildDelegateSubBackend(memory.NewMemoryManager(nil), parent, config.DelegationConfig{}, nil, nil, 1)
		},
		"batch": func(parent *tools.Dispatcher) kernel.AgentBackend {
			return NewMultiAgentHost(parent, nil, memory.NewMemoryManager(nil), nil, 1, 1, 1).buildSubBackend(nil)
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
