//go:build !windows

package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"selfmind/internal/kernel"
	"selfmind/internal/tools/sandbox"
	"strings"
	"testing"
	"time"
)

// Exercise the real tool bodies: changing the workspace does not let another
// Run write while the person's host command owns execution.
func TestConcurrentHostExecutionBlocksManagedWritesAndCommands(t *testing.T) {
	withExecSandboxPolicy(t, false, false, false)
	for _, tool := range []string{"write_file", "patch", "terminal"} {
		t.Run(tool, func(t *testing.T) {
			runtime := NewExecutionRuntime("lane-test", t.TempDir(), sandbox.PrivateState{}, nil)
			root, other := t.TempDir(), t.TempDir()
			for _, run := range []string{"host-" + tool, "other-" + tool} {
				path := root
				if run == "other-"+tool {
					path = other
				}
				t.Cleanup(SetExecutionScope("person-"+tool, ExecutionScope{TenantID: "tenant", PersonID: "person-" + tool, RunID: run, WorkspaceRoot: path, AllowedRoots: []string{path}, Runtime: runtime}))
			}
			hostArgs := laneTestArgs("host-"+tool, root)
			hostCtx, cancelHost := context.WithCancel(contextFromArgs(hostArgs))
			defer cancelHost()
			hostArgs["_context"] = hostCtx
			hostArgs["command"] = "touch started; while [ ! -f release ]; do sleep .01; done"
			hostArgs["sandbox"] = "host"
			hostDone := make(chan error, 1)
			go func() { _, err := NewExecuteCommandTool().Execute(hostArgs); hostDone <- err }()
			defer os.WriteFile(filepath.Join(root, "release"), nil, 0600)
			waitLaneFile(t, filepath.Join(root, "started"))
			args := laneTestArgs("other-"+tool, other)
			args["path"] = filepath.Join(other, "result")
			args["content"] = "result"
			args["patch"] = "*** Begin Patch\n*** Add File: " + filepath.Join(other, "result") + "\n+result\n*** End Patch"
			args["command"] = "printf result > result"
			args["sandbox"] = "host"
			done := make(chan error, 1)
			go func() {
				var err error
				switch tool {
				case "write_file":
					_, err = NewWriteFileTool().Execute(args)
				case "patch":
					_, err = NewPatchTool().Execute(args)
				case "terminal":
					_, err = NewExecuteCommandTool().Execute(args)
				}
				done <- err
			}()
			select {
			case err := <-done:
				t.Fatalf("%s escaped the host lane: %v", tool, err)
			case <-time.After(50 * time.Millisecond):
			}
			if err := os.WriteFile(filepath.Join(root, "release"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-hostDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("host did not finish")
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("managed work did not wake")
			}
		})
	}
}
func laneTestArgs(run, root string) map[string]interface{} {
	return map[string]interface{}{"_context": WithExecutionScopeKey(context.Background(), ExecutionScopeKeyForRun(run)), "cwd": root, "_tool_name": "terminal"}
}
func waitLaneFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("command did not create %s", path)
}

func TestParallelHostExecutionFollowsPolicyInsteadOfCapacity(t *testing.T) {
	withExecSandboxPolicy(t, false, false, false)
	root := t.TempDir()
	t.Cleanup(SetExecutionScope("host-policy", ExecutionScope{TenantID: "tenant", PersonID: "host-policy", RunID: "host-policy", WorkspaceRoot: root, AllowedRoots: []string{root}, ParallelWork: true, Runtime: NewExecutionRuntime("host-policy", t.TempDir(), sandbox.PrivateState{}, nil)}))
	args := laneTestArgs("host-policy", root)
	args["command"] = "printf allowed"
	args["sandbox"] = "host"
	exec := WorkspaceScopeMiddleware()(func(args map[string]interface{}) (string, error) { return NewExecuteCommandTool().Execute(args) })
	output, err := exec(args)
	if err != nil || output != "allowed" {
		t.Fatalf("parallel capacity changed host policy: %q %v", output, err)
	}
}

func TestHostSmartApprovalJudgesEveryInvocationWithoutReuse(t *testing.T) {
	withExecSandboxPolicy(t, false, false, false)
	for _, parallel := range []bool{false, true} {
		t.Run(map[bool]string{false: "serial", true: "parallel"}[parallel], func(t *testing.T) {
			judge := &fakeJudge{reply: `{"outcome":"approve","risk_level":"low","user_authorization":"high","rationale":"asked for"}`}
			run := "host-judge-" + map[bool]string{false: "serial", true: "parallel"}[parallel]
			t.Cleanup(SetExecutionScope(run, ExecutionScope{TenantID: "tenant", PersonID: run, RunID: run, TaskID: "task-host", WorkspaceID: "workspace", ParallelWork: parallel, ApprovalMode: ApprovalSmart, Judge: judge}))
			ran := 0
			exec := SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) { ran++; return "ran", nil })
			root := t.TempDir()
			for i := 0; i < 2; i++ {
				args := laneTestArgs(run, root)
				args["command"] = "printf done"
				args["sandbox"] = "host"
				if _, err := exec(args); err != nil {
					t.Fatal(err)
				}
			}
			if judge.calls != 2 || ran != 2 {
				t.Fatalf("host decision reused: judge=%d ran=%d", judge.calls, ran)
			}
		})
	}
}

func TestConcurrentExecutionLaneDoesNotBlockAnotherPersonOrRuntime(t *testing.T) {
	withExecSandboxPolicy(t, false, false, false)
	first := NewExecutionRuntime("first", t.TempDir(), sandbox.PrivateState{}, nil)
	second := NewExecutionRuntime("second", t.TempDir(), sandbox.PrivateState{}, nil)
	release, err := first.lane(executionLaneKey{"tenant", "person"}).acquire(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for _, tc := range []struct {
		name, person string
		runtime      *ExecutionRuntime
	}{{"other-person", "other", first}, {"other-runtime", "person", second}} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			run := "separate-" + tc.name
			t.Cleanup(SetExecutionScope(run, ExecutionScope{TenantID: "tenant", PersonID: tc.person, RunID: run, WorkspaceRoot: root, AllowedRoots: []string{root}, Runtime: tc.runtime}))
			args := laneTestArgs(run, root)
			args["path"] = filepath.Join(root, "result")
			args["content"] = "done"
			done := make(chan error, 1)
			go func() { _, err := NewWriteFileTool().Execute(args); done <- err }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("unrelated execution was blocked")
			}
		})
	}
}

func TestExecutionLaneCancellationReleasesWaiterAndHostFailure(t *testing.T) {
	withExecSandboxPolicy(t, false, false, false)
	runtime := NewExecutionRuntime("failure", t.TempDir(), sandbox.PrivateState{}, nil)
	key := executionLaneKey{"tenant", "person"}
	release, _ := runtime.lane(key).acquire(context.Background(), false)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := runtime.lane(key).acquire(ctx, true); done <- err }()
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("cancel=%v", err)
	}
	release()
	root := t.TempDir()
	t.Cleanup(SetExecutionScope("lane-failure", ExecutionScope{TenantID: key.tenant, PersonID: key.person, RunID: "lane-failure", WorkspaceRoot: root, AllowedRoots: []string{root}, Runtime: runtime}))
	args := laneTestArgs("lane-failure", root)
	args["command"] = "exit 3"
	args["sandbox"] = "host"
	if _, err := NewExecuteCommandTool().Execute(args); err == nil {
		t.Fatal("real command did not fail")
	}
	if !runtime.ExecutionLaneReady(key.tenant, key.person) {
		t.Fatal("failed command retained execution lane")
	}
}

func TestHostApprovalPolicyHasNoReusableChoicesAndKeepsHardFloor(t *testing.T) {
	withExecSandboxPolicy(t, false, false, false)
	for _, mode := range []ApprovalMode{ApprovalOnRequest, ApprovalSmart, ApprovalFullAuto} {
		t.Run(string(mode), func(t *testing.T) {
			run := "host-mode-" + string(mode)
			asks := 0
			judge := &fakeJudge{err: context.DeadlineExceeded}
			t.Cleanup(SetExecutionScope(run, ExecutionScope{TenantID: "tenant", PersonID: run, RunID: run, ApprovalMode: mode, Judge: judge, Approval: func(_ context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
				asks++
				if mode == ApprovalSmart && (req.DecisionPolicy != ApprovalDecisionPolicyOnceOnly || req.GrantClass != "" || req.RunGrantClass != "" || len(req.RuleCandidates) > 0) {
					t.Fatalf("host request offered reuse: %+v", req)
				}
				return ToolApprovalDecision{Approved: true}, nil
			}}))
			ran := 0
			exec := SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) { ran++; return "ran", nil })
			for i := 0; i < 2; i++ {
				args := laneTestArgs(run, "/tmp")
				args["command"] = "printf approved"
				args["sandbox"] = "host"
				if _, err := exec(args); err != nil {
					t.Fatal(err)
				}
			}
			want := 2
			if mode == ApprovalFullAuto {
				want = 0
			}
			if asks != want {
				t.Fatalf("asks=%d want %d", asks, want)
			}
			args := laneTestArgs(run, "/tmp")
			args["command"] = "rm -rf /"
			args["sandbox"] = "host"
			if _, err := exec(args); err == nil || ran != 2 {
				t.Fatal("host/full-auto bypassed hard safety floor")
			}
		})
	}
}

func TestExplicitHostBackgroundReleasesLaneAfterStartupAndWarns(t *testing.T) {
	withExecSandboxPolicy(t, false, false, false)
	runtime := NewExecutionRuntime("background", t.TempDir(), sandbox.PrivateState{}, nil)
	root := t.TempDir()
	run := "lane-background"
	t.Cleanup(SetExecutionScope(run, ExecutionScope{TenantID: "tenant", PersonID: "person", RunID: run, WorkspaceRoot: root, AllowedRoots: []string{root}, Runtime: runtime}))
	args := laneTestArgs(run, root)
	args["command"] = "touch started; while [ ! -f release ]; do sleep .01; done"
	args["sandbox"], args["background"] = "host", true
	output, err := NewExecuteCommandTool().Execute(args)
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(output, "\n", 2)[0], "Started background process with ID:"))
	registry := ProcessRegistryForArgs(args)
	registry.mu.RLock()
	process := registry.processes[id]
	registry.mu.RUnlock()
	if process == nil {
		t.Fatalf("background process not registered: %q", output)
	}
	t.Cleanup(func() {
		_ = registry.Kill(id)
		select {
		case <-process.finished:
		case <-time.After(5 * time.Second):
			t.Error("test background process was not reaped")
		}
	})
	waitLaneFile(t, filepath.Join(root, "started"))
	if !strings.Contains(output, "may still affect files used by other Runs") || !runtime.ExecutionLaneReady("tenant", "person") {
		t.Fatalf("background did not warn/release: %q", output)
	}
	args = laneTestArgs(run, root)
	args["path"], args["content"] = filepath.Join(root, "result"), "another write"
	if _, err := NewWriteFileTool().Execute(args); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestManagedExecutionReleasesLaneOnPanicAndDoesNotReuseStaleContext(t *testing.T) {
	runtime := NewExecutionRuntime("panic", t.TempDir(), sandbox.PrivateState{}, nil)
	run, root := "lane-panic", t.TempDir()
	t.Cleanup(SetExecutionScope(run, ExecutionScope{TenantID: "tenant", PersonID: "person", RunID: run, WorkspaceRoot: root, Runtime: runtime}))
	args := laneTestArgs(run, root)
	args["_tool_name"] = "write_file"
	args["path"] = filepath.Join(root, "result")
	exec := ManagedExecutionMiddleware()(func(map[string]interface{}) (kernel.ToolDispatchResult, error) { panic("body panic") })
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic swallowed")
			}
		}()
		_, _ = exec(args)
	}()
	if !runtime.ExecutionLaneReady("tenant", "person") {
		t.Fatal("panic retained lane")
	}
	stale, release, err := acquireManagedExecution(contextFromArgs(args), args, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	release()
	held, _ := runtime.lane(executionLaneKey{"tenant", "person"}).acquire(context.Background(), true)
	defer held()
	ctx, cancel := context.WithTimeout(stale, 20*time.Millisecond)
	defer cancel()
	args["_context"] = ctx
	if _, _, err := acquireManagedExecution(ctx, args, nil, false); err != context.DeadlineExceeded {
		t.Fatalf("stale context bypassed lane: %v", err)
	}
}

func TestParallelApprovalContextIsBoundedObservedDataAndFailsToHuman(t *testing.T) {
	withExecSandboxPolicy(t, false, false, false)
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "facts", true: "unavailable"}[failed], func(t *testing.T) {
			judge := &fakeJudge{reply: `{"outcome":"approve","risk_level":"low","user_authorization":"high","rationale":"requested"}`}
			asked := 0
			scope := ExecutionScope{TenantID: "tenant", PersonID: "person", TaskID: "task", RunID: "run-parallel-context", ApprovalMode: ApprovalSmart, Judge: judge, ParallelContext: func(context.Context) ([]ParallelExecutionContext, error) {
				if failed {
					return nil, fmt.Errorf("injected read failure")
				}
				return []ParallelExecutionContext{{RunID: "other-run", Directories: []string{"/real/workspace", strings.Repeat("界", 2048), "omitted-dir"}, InFlightTargets: []string{"service:actual-target"}}}, nil
			}, Approval: func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error) {
				asked++
				return ToolApprovalDecision{Approved: true}, nil
			}}
			t.Cleanup(SetExecutionScope("person", scope))
			args := laneTestArgs(scope.RunID, t.TempDir())
			args["command"], args["sandbox"] = "printf facts", "host"
			exec := SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) { return "ran", nil })
			if _, err := exec(args); err != nil {
				t.Fatal(err)
			}
			if failed {
				if judge.calls != 0 || asked != 1 {
					t.Fatalf("missing state authorized: judge=%d asked=%d", judge.calls, asked)
				}
				return
			}
			if judge.calls != 1 || asked != 0 || !strings.Contains(judge.lastArg, "/real/workspace") || !strings.Contains(judge.lastArg, "service:actual-target") || !strings.Contains(judge.lastArg, "never authorization") || strings.Contains(judge.lastArg, "omitted-dir") || len(judge.lastArg) > 10000 {
				t.Fatalf("bad bounded context: judge=%d asks=%d prompt=%q", judge.calls, asked, judge.lastArg)
			}
		})
	}
}

func TestExternalOccupancyWaitDoesNotOwnExecutionLane(t *testing.T) {
	f := newEffectLifecycleFixture(t, 2)
	owner, waiter := f.runs[0], f.runs[1]
	inFlight(t, f, owner, "deploy")
	scope, _ := currentExecutionScopeAny(map[string]interface{}{"_invocation_scope": kernel.ToolInvocationScope{ExecutionScopeKey: ExecutionScopeKeyForRun(waiter.ID)}})
	entered := make(chan struct{})
	f.registry = NewRegistry()
	f.registry.UseResultMiddleware(func(next ResultExecutor) ResultExecutor {
		return func(args map[string]interface{}) (kernel.ToolDispatchResult, error) {
			close(entered)
			return next(args)
		}
	})
	f.registry.UseResultMiddleware(externalEffectClaimMiddleware(f.store, 5*time.Second))
	f.registry.UseResultMiddleware(ManagedExecutionMiddleware())
	f.registry.Register(newLifecycleExecTool("terminal", func(string) (kernel.ToolDispatchResult, error) { return exitedResult(0), nil }))
	done := make(chan error, 1)
	go func() {
		_, err := f.dispatch(waiter, "terminal", "push", map[string]interface{}{"command": "git push origin main", "_network_shared": true})
		done <- err
	}()
	<-entered
	select {
	case err := <-done:
		t.Fatalf("occupied target did not wait: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if !scope.Runtime.ExecutionLaneReady(f.owner.TenantID, f.owner.PersonID) {
		t.Fatal("external target waiter owns host lane")
	}
	args := laneTestArgs(waiter.ID, f.roots[waiter.ID])
	args["command"], args["sandbox"] = "printf independent", "host"
	ctx, cancel := context.WithTimeout(contextFromArgs(args), time.Second)
	defer cancel()
	args["_context"] = ctx
	if output, err := NewExecuteCommandTool().Execute(args); err != nil || output != "independent" {
		t.Fatalf("local execution blocked behind target wait: %q %v", output, err)
	}
	if err := f.store.ObserveExternalEffectGroup(context.Background(), f.owner.TenantID, f.owner.PersonID, owner.ID, "deploy", "test:observed"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("effect waiter did not wake")
	}
}
