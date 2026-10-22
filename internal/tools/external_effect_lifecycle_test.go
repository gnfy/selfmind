package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/tools/sandbox"
)

// lifecycleExecTool stands in for an exec tool whose process facts a test
// chooses. Its name makes the claim middleware treat it as a shell command.
type lifecycleExecTool struct {
	BaseTool
	mu      sync.Mutex
	calls   []string
	respond func(command string) (kernel.ToolDispatchResult, error)
}

func newLifecycleExecTool(name string, respond func(string) (kernel.ToolDispatchResult, error)) *lifecycleExecTool {
	tool := &lifecycleExecTool{respond: respond}
	tool.BaseTool = BaseTool{name: name, description: "lifecycle exec", schema: ToolSchema{Type: "object",
		Properties: map[string]PropertyDef{"command": {Type: "string"}, "cwd": {Type: "string"}}}}
	return tool
}

func (t *lifecycleExecTool) Execute(args map[string]interface{}) (string, error) {
	result, err := t.ExecuteResult(args)
	return result.Output, err
}

func (t *lifecycleExecTool) ExecuteResult(args map[string]interface{}) (kernel.ToolDispatchResult, error) {
	command := stringArg(args, "command")
	t.mu.Lock()
	t.calls = append(t.calls, command)
	t.mu.Unlock()
	return t.respond(command)
}

func (t *lifecycleExecTool) commands() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.calls...)
}

func exitedResult(code int) kernel.ToolDispatchResult {
	return kernel.ToolDispatchResult{Output: fmt.Sprintf("exit %d", code), Process: &kernel.ToolProcessResult{Started: true, ExitCode: &code}}
}

// externalLifecycleTool is a synchronous tool from an external server, like an
// MCP tool: no local process facts and no trusted target adapter.
type externalLifecycleTool struct {
	BaseTool
	mu    sync.Mutex
	calls int
	fail  error
}

func (*externalLifecycleTool) SchemaOrigin() ToolSchemaOrigin { return ToolSchemaOriginExternal }

func (t *externalLifecycleTool) Execute(map[string]interface{}) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls++
	if t.fail != nil {
		return "", t.fail
	}
	return "remote call finished", nil
}

func newExternalLifecycleTool(name string) *externalLifecycleTool {
	return &externalLifecycleTool{BaseTool: BaseTool{name: name, description: "external lifecycle", schema: ToolSchema{Type: "object",
		Properties: map[string]PropertyDef{"text": {Type: "string"}}}}}
}

type effectLifecycleFixture struct {
	t        *testing.T
	ctx      context.Context
	store    *control.Store
	owner    *control.IdentityContext
	runs     []*control.Run
	roots    map[string]string
	registry *Registry
}

func newEffectLifecycleFixture(t *testing.T, runCount int) *effectLifecycleFixture {
	t.Helper()
	withExecSandboxPolicy(t, false, false, false)
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "lifecycle-owner", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	f := &effectLifecycleFixture{t: t, ctx: ctx, store: store, owner: owner, registry: NewRegistry(), roots: map[string]string{}}
	f.registry.UseResultMiddleware(ExternalEffectClaimMiddleware(store))
	runtime := NewExecutionRuntime("effects", t.TempDir(), sandbox.PrivateState{}, nil)
	for i := 0; i < runCount; i++ {
		title := fmt.Sprintf("work-%d", i)
		task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: title, Channel: title})
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.StartRunWithOptions(ctx, task, title, title, control.StartRunOptions{MaxActiveRuns: 3})
		if err != nil {
			t.Fatal(err)
		}
		f.runs = append(f.runs, run)
		root := t.TempDir()
		f.roots[run.ID] = root
		cleanup := SetExecutionScope(ExecutionScopeKeyForRun(run.ID), ExecutionScope{TenantID: owner.TenantID,
			PersonID: owner.PersonID, RunID: run.ID, TaskID: run.TaskID, WorkspaceID: "workspace", WorkspaceRoot: root,
			AllowedRoots: []string{root}, ParallelWork: true, Runtime: runtime})
		t.Cleanup(cleanup)
	}
	return f
}

func (f *effectLifecycleFixture) dispatch(run *control.Run, tool, call string, args map[string]interface{}) (kernel.ToolDispatchResult, error) {
	f.t.Helper()
	// Commands run in the Run's own workspace, never the test's directory.
	merged := map[string]interface{}{
		"_tool_call_id":     call,
		"_invocation_scope": kernel.ToolInvocationScope{ExecutionScopeKey: ExecutionScopeKeyForRun(run.ID)},
	}
	if _, isShell := args["command"]; isShell {
		merged["cwd"] = f.roots[run.ID]
	}
	for key, value := range args {
		merged[key] = value
	}
	return f.registry.DispatchResult(tool, merged)
}

func (f *effectLifecycleFixture) unresolved() []control.ExternalEffectClaim {
	f.t.Helper()
	claims, err := f.store.ListUnresolvedExternalEffects(f.ctx, f.owner.TenantID, f.owner.PersonID, 100)
	if err != nil {
		f.t.Fatal(err)
	}
	return claims
}

// A foreground command that exits 0 and a synchronous external tool that
// returns success settle their claims when the call returns, so the next
// conflicting effect of any Run dispatches.
func TestSuccessfulReturnSettlesExternalEffectClaim(t *testing.T) {
	for _, tc := range []struct {
		name, command string
	}{
		{"exit zero", "touch done.txt"},
		{"other command", "mkdir -p build && touch build/ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEffectLifecycleFixture(t, 2)
			f.registry.Register(NewExecuteCommandTool())
			if _, err := f.dispatch(f.runs[0], "terminal", "call-ok", map[string]interface{}{"command": tc.command, "_network_shared": true}); err != nil {
				t.Fatalf("successful command: %v", err)
			}
			if left := f.unresolved(); len(left) != 0 {
				t.Fatalf("exit 0 left the claim unresolved: %+v", left)
			}
			if _, err := f.dispatch(f.runs[1], "terminal", "call-next", map[string]interface{}{"command": tc.command, "_network_shared": true}); err != nil {
				t.Fatalf("another Run stayed blocked by a settled claim: %v", err)
			}
		})
	}
	t.Run("synchronous external tool", func(t *testing.T) {
		f := newEffectLifecycleFixture(t, 2)
		mcp := newExternalLifecycleTool("mcp_stub_echo")
		f.registry.Register(mcp)
		for i, run := range []*control.Run{f.runs[0], f.runs[0], f.runs[1]} {
			if _, err := f.dispatch(run, mcp.Name(), fmt.Sprintf("echo-%d", i), map[string]interface{}{"text": "hello"}); err != nil {
				t.Fatalf("external call %d: %v", i, err)
			}
		}
		if mcp.calls != 3 {
			t.Fatalf("external tool calls = %d, want 3", mcp.calls)
		}
		if left := f.unresolved(); len(left) != 0 {
			t.Fatalf("successful external tool stranded its claim: %+v", left)
		}
	})
}

// Every unsuccessful or unproven return keeps the claim, and the failing
// call's own result tells the model how to release it.
func TestUnsuccessfulReturnKeepsClaimAndExplainsRelease(t *testing.T) {
	cases := []struct {
		name    string
		tool    string
		args    map[string]interface{}
		cancel  bool
		outcome string
	}{
		{name: "non-zero exit", tool: "terminal", args: map[string]interface{}{"command": "exit 3"}, outcome: "exit 3"},
		{name: "non-zero exit, other wording", tool: "terminal", args: map[string]interface{}{"command": "sh -c 'exit 7'"}, outcome: "exit 7"},
		{name: "timeout", tool: "terminal", args: map[string]interface{}{"command": "touch started && sleep 5", "timeout": 1}, outcome: "no exit status"},
		{name: "cancelled", tool: "terminal", args: map[string]interface{}{"command": "touch started && sleep 5"}, cancel: true, outcome: "no exit status"},
		{name: "external tool error", tool: "mcp_stub_echo", args: map[string]interface{}{"text": "x"}, outcome: "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newEffectLifecycleFixture(t, 1)
			f.registry.Register(NewExecuteCommandTool())
			mcp := newExternalLifecycleTool("mcp_stub_echo")
			mcp.fail = errors.New("tool mcp_stub_echo: remote rejected the request")
			f.registry.Register(mcp)
			args := map[string]interface{}{"_network_shared": true}
			for key, value := range tc.args {
				args[key] = value
			}
			if tc.cancel {
				ctx, cancel := context.WithCancel(context.Background())
				args["_context"] = ctx
				timer := time.AfterFunc(300*time.Millisecond, cancel)
				defer timer.Stop()
			}
			_, err := f.dispatch(f.runs[0], tc.tool, "call-failed", args)
			if err == nil {
				t.Fatal("unsuccessful call reported success")
			}
			left := f.unresolved()
			if len(left) != 1 || left[0].State != control.ExternalClaimUncertain {
				t.Fatalf("claim after %s = %+v", tc.name, left)
			}
			for _, want := range []string{left[0].ID, "unresolved", "effect_claim_id", "/effects settle " + left[0].ID, tc.outcome} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("failing result does not say %q: %v", want, err)
				}
			}
			var facts interface{ ModelSafeMessage() string }
			if errors.As(err, &facts) && !strings.Contains(facts.ModelSafeMessage(), left[0].ID) {
				t.Fatalf("model-visible message omits the claim: %q", facts.ModelSafeMessage())
			}
		})
	}
}

// Two networked commands in one Run both dispatch: the first one's exit 0
// settled its claim, so the Run never waits on itself.
func TestSameRunSequentialNetworkedEffectsBothDispatch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first string
		then  string
	}{
		{"install then push", "npm install", "git push origin main"},
		{"other wording", "pip install -r requirements.txt", "gh release create v1.2.0 --notes ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEffectLifecycleFixture(t, 1)
			shell := newLifecycleExecTool("terminal", func(string) (kernel.ToolDispatchResult, error) { return exitedResult(0), nil })
			f.registry.Register(shell)
			for i, command := range []string{tc.first, tc.then} {
				if _, err := f.dispatch(f.runs[0], "terminal", fmt.Sprintf("call-%d", i), map[string]interface{}{"command": command, "_network_shared": true}); err != nil {
					t.Fatalf("%q was not dispatched: %v", command, err)
				}
			}
			if got := shell.commands(); len(got) != 2 {
				t.Fatalf("dispatched commands = %v", got)
			}
			if left := f.unresolved(); len(left) != 0 {
				t.Fatalf("settled effects left claims: %+v", left)
			}
		})
	}
	t.Run("external tool then command", func(t *testing.T) {
		f := newEffectLifecycleFixture(t, 1)
		mcp := newExternalLifecycleTool("mcp_stub_echo")
		shell := newLifecycleExecTool("terminal", func(string) (kernel.ToolDispatchResult, error) { return exitedResult(0), nil })
		f.registry.Register(mcp)
		f.registry.Register(shell)
		if _, err := f.dispatch(f.runs[0], mcp.Name(), "echo", map[string]interface{}{"text": "x"}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.dispatch(f.runs[0], "terminal", "push", map[string]interface{}{"command": "git push", "_network_shared": true}); err != nil {
			t.Fatalf("command after a successful external tool: %v", err)
		}
	})
}

// A background start never settles at start: the claim is held while the
// process runs and settled only when the daemon observes exit status 0.
func TestBackgroundStartSettlesOnlyOnObservedExitZero(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		settled bool
	}{
		// The process runs long enough to be seen running even on a loaded
		// host.
		{"exit zero", "touch bg-started; sleep 2; exit 0", true},
		{"non-zero exit", "touch bg-started; sleep 2; exit 4", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEffectLifecycleFixture(t, 1)
			f.registry.Register(NewExecuteCommandTool())
			result, err := f.dispatch(f.runs[0], "terminal", "call-background", map[string]interface{}{
				"command": tc.command, "background": true, "sandbox": "host", "_network_shared": true,
			})
			if err != nil || result.Process == nil || !result.Process.Started {
				t.Fatalf("background start: %+v %v", result, err)
			}
			if left := f.unresolved(); len(left) != 1 {
				t.Fatalf("background start settled or lost its claim at start: %+v", left)
			}
			effect := kernel.ToolEffectID(f.runs[0].ID, "call-background")
			deadline := time.Now().Add(30 * time.Second)
			for {
				claims, err := f.store.ExternalEffectClaimsForEffect(f.ctx, f.owner.TenantID, f.owner.PersonID, f.runs[0].ID, effect)
				if err != nil || len(claims) != 1 {
					t.Fatalf("claims=%+v err=%v", claims, err)
				}
				claim := claims[0]
				if claim.State == control.ExternalClaimObserved {
					if !tc.settled || !strings.HasPrefix(claim.ObservationRef, "process:exited:0:") {
						t.Fatalf("background exit settled wrongly: %+v", claim)
					}
					return
				}
				if claim.Disposition == control.ExternalClaimLeftUncertain {
					if tc.settled || !strings.Contains(claim.Outcome, "exit 4") {
						t.Fatalf("background exit left the claim uncertain wrongly: %+v", claim)
					}
					return
				}
				if claim.Disposition != control.ExternalClaimInFlight {
					t.Fatalf("running background process is not in flight: %+v", claim)
				}
				if time.Now().After(deadline) {
					t.Fatalf("background exit was never recorded: %+v", claim)
				}
				time.Sleep(50 * time.Millisecond)
			}
		})
	}
}

// The settling observation names the exact call that settled it.
func TestSettledClaimObservationNamesTheCall(t *testing.T) {
	for _, call := range []string{"call-named", "other-call-id"} {
		f := newEffectLifecycleFixture(t, 1)
		f.registry.Register(NewExecuteCommandTool())
		if _, err := f.dispatch(f.runs[0], "terminal", call, map[string]interface{}{"command": "touch named", "_network_shared": true}); err != nil {
			t.Fatal(err)
		}
		claims, err := f.store.ExternalEffectClaimsForEffect(f.ctx, f.owner.TenantID, f.owner.PersonID, f.runs[0].ID, kernel.ToolEffectID(f.runs[0].ID, call))
		if err != nil || len(claims) != 1 {
			t.Fatalf("claims=%+v err=%v", claims, err)
		}
		if claims[0].State != control.ExternalClaimObserved || claims[0].ObservationRef != "tool:completed:"+call {
			t.Fatalf("settlement did not name the call: %+v", claims[0])
		}
	}
}

// A failed call is no longer in flight once it returns, and its dispatch
// record keeps a redacted summary of what ran.
func TestFailedCallIsLeftUncertainWithItsSummaryAndExitStatus(t *testing.T) {
	f := newEffectLifecycleFixture(t, 1)
	shell := newLifecycleExecTool("terminal", func(string) (kernel.ToolDispatchResult, error) {
		return exitedResult(2), errors.New("command failed: exit status 2")
	})
	f.registry.Register(shell)
	RegisterSensitiveValue("sk-lifecycle-secret-0123456789")
	_, err := f.dispatch(f.runs[0], "terminal", "call-secret", map[string]interface{}{
		"command": "curl -H 'Authorization: Bearer sk-lifecycle-secret-0123456789' -X POST https://deploy.example.test/release", "_network_shared": true,
	})
	if err == nil {
		t.Fatal("failed call reported success")
	}
	claims, err := f.store.ExternalEffectClaimsForEffect(f.ctx, f.owner.TenantID, f.owner.PersonID, f.runs[0].ID, kernel.ToolEffectID(f.runs[0].ID, "call-secret"))
	if err != nil || len(claims) != 1 {
		t.Fatalf("claims=%+v err=%v", claims, err)
	}
	claim := claims[0]
	if claim.Disposition != control.ExternalClaimLeftUncertain || claim.Outcome != "exit 2" ||
		!strings.Contains(claim.Summary, "deploy.example.test/release") || strings.Contains(claim.Summary, "sk-lifecycle-secret") {
		t.Fatalf("returned failure facts: %+v", claim)
	}
}

// Review round 2, item 2: the owner Run may end while its background process
// still runs. The claim stays in flight, so nothing releases it early, and
// the process's later non-zero exit is still recorded.
func TestBackgroundProcessOutlivingItsRunStaysInFlightUntilExit(t *testing.T) {
	for _, finish := range []string{"done", "interrupted"} {
		t.Run(finish, func(t *testing.T) {
			f := newEffectLifecycleFixture(t, 1)
			f.registry.Register(NewExecuteCommandTool())
			run := f.runs[0]
			if _, err := f.dispatch(run, "terminal", "call-serve", map[string]interface{}{
				// Long enough that the process outlives its Run's end even on a
				// loaded host.
				"command": "touch serving; sleep 3; exit 3", "background": true, "sandbox": "host", "_network_shared": true,
			}); err != nil {
				t.Fatalf("background start: %v", err)
			}
			if err := f.store.FinishRun(f.ctx, f.owner.TenantID, run.ID, finish); err != nil {
				t.Fatal(err)
			}
			effect := kernel.ToolEffectID(run.ID, "call-serve")
			claims, err := f.store.ExternalEffectClaimsForEffect(f.ctx, f.owner.TenantID, f.owner.PersonID, run.ID, effect)
			if err != nil || claims[0].Disposition != control.ExternalClaimInFlight {
				t.Fatalf("a running background process whose Run ended: %+v %v", claims, err)
			}
			deadline := time.Now().Add(30 * time.Second)
			for {
				claims, err = f.store.ExternalEffectClaimsForEffect(f.ctx, f.owner.TenantID, f.owner.PersonID, run.ID, effect)
				if err != nil {
					t.Fatal(err)
				}
				if claims[0].Disposition == control.ExternalClaimLeftUncertain {
					if claims[0].Outcome != "background process exit 3" {
						t.Fatalf("recorded exit: %+v", claims[0])
					}
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("the process exit was never recorded: %+v", claims[0])
				}
				time.Sleep(50 * time.Millisecond)
			}
		})
	}
}
