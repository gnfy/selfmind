//go:build !windows

package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"selfmind/internal/kernel"
	"selfmind/internal/tools/sandbox"
)

func TestExecutionLaneTokenBindsRuntimeRunLeaseAndCall(t *testing.T) {
	for _, boundary := range []string{"same-invocation", "run", "lease", "call", "runtime"} {
		t.Run(boundary, func(t *testing.T) {
			runtime := NewExecutionRuntime("token-owner", t.TempDir(), sandbox.PrivateState{}, nil)
			root := t.TempDir()
			scope := ExecutionScope{TenantID: "tenant", PersonID: "person", RunID: "token-owner", LeaseID: "lease-a", WorkspaceRoot: root, AllowedRoots: []string{root}, Runtime: runtime, ParallelWork: true}
			t.Cleanup(SetExecutionScope(scope.PersonID, scope))
			owner := laneTestArgs(scope.RunID, root)
			owner["_tool_call_id"] = "call-a"
			inherited, release, err := acquireManagedExecution(contextFromArgs(owner), owner, nil, true)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			next := scope
			call := "call-a"
			switch boundary {
			case "run":
				next.RunID = "token-other-run"
			case "lease":
				next.LeaseID = "lease-b"
			case "call":
				call = "call-b"
			case "runtime":
				next.Runtime = NewExecutionRuntime("token-other-runtime", t.TempDir(), sandbox.PrivateState{}, nil)
				held, err := next.Runtime.lane(executionLaneKey{next.TenantID, next.PersonID}).acquire(context.Background(), true)
				if err != nil {
					t.Fatal(err)
				}
				defer held()
			}
			t.Cleanup(SetExecutionScope(next.PersonID, next))
			ctx, cancel := context.WithTimeout(WithExecutionScopeKey(inherited, ExecutionScopeKeyForRun(next.RunID)), 25*time.Millisecond)
			defer cancel()
			path := filepath.Join(root, "result")
			args := map[string]interface{}{"_context": ctx, "_tool_name": "write_file", "_tool_call_id": call, "path": path, "content": "effect"}
			exec := WorkspaceScopeMiddleware()(func(args map[string]interface{}) (string, error) { return NewWriteFileTool().Execute(args) })
			_, err = exec(args)
			_, statErr := os.Stat(path)
			if boundary == "same-invocation" {
				if err != nil || statErr != nil {
					t.Fatalf("same tool body could not reenter: %v %v", err, statErr)
				}
			} else if err == nil || !os.IsNotExist(statErr) {
				t.Fatalf("%s boundary borrowed another active token: err=%v stat=%v", boundary, err, statErr)
			}
		})
	}
}

func TestParkedHostExecutionRetainsFIFOIntentWithoutBlockingItsOwnResume(t *testing.T) {
	runtime := NewExecutionRuntime("fairness", t.TempDir(), sandbox.PrivateState{}, nil)
	key := executionLaneKey{"tenant", "person"}
	lane := runtime.lane(key)
	// Two already-entered shared calls overlap. The host expires its inline grace.
	first, _ := lane.acquire(context.Background(), false)
	second, _ := lane.acquire(context.Background(), false)
	defer first()
	defer second()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	_, err := lane.acquire(ctx, true)
	cancel()
	if err != context.DeadlineExceeded {
		t.Fatalf("host grace=%v", err)
	}
	runtime.PreserveQueuedHostExecution(key.tenant, key.person, "host-first")
	runtime.PreserveQueuedHostExecution(key.tenant, key.person, "host-second")
	runtime.ReconcileQueuedHostExecution(key.tenant, key.person, []string{"host-first", "host-second"})
	first()
	for _, host := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		release, err := lane.acquire(ctx, host)
		cancel()
		if release != nil {
			release()
		}
		if err != context.DeadlineExceeded {
			t.Fatalf("new host=%v barged into overlapping shared calls: %v", host, err)
		}
	}
	second()
	if !runtime.ExecutionLaneReady(key.tenant, key.person, "host-first") || runtime.ExecutionLaneReady(key.tenant, key.person, "host-second") || runtime.ExecutionLaneReady(key.tenant, key.person) {
		t.Fatal("own queued intent prevented resume or a later dispatch was ready first")
	}
	for _, ref := range []string{"host-first", "host-second"} {
		if !runtime.ExecutionLaneReady(key.tenant, key.person, ref) {
			t.Fatalf("%s could not wake", ref)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		release, err := lane.acquire(ctx, true, laneAcquisition{ref: ref})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		// Claimed child remains present in durable facts, but its consumed intent
		// cannot reappear and deadlock its own execution.
		runtime.ReconcileQueuedHostExecution(key.tenant, key.person, []string{"host-first", "host-second"})
		release()
		if ref == "host-first" {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
			release, err := lane.acquire(ctx, true)
			cancel()
			if release != nil {
				release()
			}
			if err != context.DeadlineExceeded {
				t.Fatalf("new host overtook second parked host: %v", err)
			}
		}
	}
	runtime.ReconcileQueuedHostExecution(key.tenant, key.person, nil)
	if !runtime.ExecutionLaneReady(key.tenant, key.person) {
		t.Fatal("consumed queue retained writer intent")
	}
}

func TestQueuedHostYieldAllowsOtherResourceOwnerToExecute(t *testing.T) {
	runtime := NewExecutionRuntime("yield", t.TempDir(), sandbox.PrivateState{}, nil)
	key := executionLaneKey{"tenant", "person"}
	runtime.ReconcileQueuedHostExecution(key.tenant, key.person, []string{"waiting-host"})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	release, err := runtime.lane(key).acquire(ctx, true)
	cancel()
	if release != nil {
		release()
	}
	if err != context.DeadlineExceeded {
		t.Fatalf("owner observation passed before yield: %v", err)
	}
	// The resumed host needs human input or another Run's effect observation.
	// Yield does not erase the CP; repeated durable reconciliation stays yielded.
	runtime.YieldQueuedHostExecution(key.tenant, key.person, "waiting-host")
	runtime.ReconcileQueuedHostExecution(key.tenant, key.person, []string{"waiting-host"})
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err = runtime.lane(key).acquire(ctx, true)
	if err != nil {
		t.Fatalf("resource owner cannot observe after yield: %v", err)
	}
	release()
	if !runtime.ExecutionLaneReady(key.tenant, key.person, "waiting-host") {
		t.Fatal("yielded exact host could not retry after resource resolved")
	}
}

func TestExecutionLaneReadFailureDoesNotParkOrDispatch(t *testing.T) {
	runtime := NewExecutionRuntime("read-failure", t.TempDir(), sandbox.PrivateState{}, nil)
	run := "lane-read-failure"
	parked := false
	t.Cleanup(SetExecutionScope(run, ExecutionScope{TenantID: "tenant", PersonID: "person", RunID: run, Runtime: runtime, RefreshExecutionWaits: func(context.Context) error { return context.DeadlineExceeded }, ParkExecutionWait: func(context.Context, string, bool) (bool, error) { parked = true; return true, nil }, ExecutionWaitGrace: 20 * time.Millisecond}))
	args := laneTestArgs(run, t.TempDir())
	args["_tool_name"] = "write_file"
	invoked := false
	exec := ManagedExecutionMiddleware()(func(map[string]interface{}) (kernel.ToolDispatchResult, error) {
		invoked = true
		return kernel.ToolDispatchResult{}, nil
	})
	if _, err := exec(args); err == nil || parked || invoked {
		t.Fatalf("read failure treated as lane expiry: %v parked=%v invoked=%v", err, parked, invoked)
	}
	// A blocked lookup honors cancellation, without inventing a saved checkpoint.
	t.Cleanup(SetExecutionScope(run, ExecutionScope{TenantID: "tenant", PersonID: "person", RunID: run, Runtime: runtime, RefreshExecutionWaits: func(ctx context.Context) error {
		<-ctx.Done()
		return fmt.Errorf("selector interrupted: %w", ctx.Err())
	}}))
	ctx, cancel := context.WithCancel(contextFromArgs(args))
	cancel()
	args["_context"] = ctx
	if _, err := exec(args); err == nil || invoked {
		t.Fatalf("cancelled selector dispatched: %v", err)
	}
}

func TestWatchRegistrationDoesNotOwnLaneDuringHumanOrDurableWait(t *testing.T) {
	runtime := NewExecutionRuntime("watch-registration", t.TempDir(), sandbox.PrivateState{}, nil)
	run := "watch-register"
	t.Cleanup(SetExecutionScope(run, ExecutionScope{TenantID: "tenant", PersonID: "person", RunID: run, Runtime: runtime}))
	args := laneTestArgs(run, t.TempDir())
	args["_tool_name"] = "watch_external"
	args["sandbox"] = "host"
	exec := ManagedExecutionMiddleware()(func(map[string]interface{}) (kernel.ToolDispatchResult, error) {
		if !runtime.ExecutionLaneReady("tenant", "person") {
			t.Fatal("registration holds lane while it may ask a human")
		}
		return kernel.ToolDispatchResult{}, nil
	})
	if _, err := exec(args); err != nil {
		t.Fatal(err)
	}
}

func TestManagedExecutionWithoutRuntimeDoesNotDispatchParallelWrite(t *testing.T) {
	root := t.TempDir()
	run := "missing-lane-runtime"
	t.Cleanup(SetExecutionScope(run, ExecutionScope{TenantID: "tenant", PersonID: "person", RunID: run, WorkspaceRoot: root, AllowedRoots: []string{root}, ParallelWork: true}))
	args := laneTestArgs(run, root)
	args["path"] = filepath.Join(root, "result")
	args["content"] = "must not write"
	if _, err := NewWriteFileTool().Execute(args); err == nil {
		t.Fatal("parallel write ignored absent managed runtime")
	}
	if _, err := os.Stat(filepath.Join(root, "result")); !os.IsNotExist(err) {
		t.Fatalf("runtime failure wrote: %v", err)
	}
}

func TestDurableWatchCheckUsesPersonsManagedExecutionLane(t *testing.T) {
	withExecSandboxPolicy(t, false, false, false)
	runtime := NewExecutionRuntime("durable-lane", t.TempDir(), sandbox.PrivateState{}, nil)
	held, err := runtime.lane(executionLaneKey{"tenant", "person"}).acquire(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer held()
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(WithExecutionRuntime(context.Background(), runtime), 25*time.Millisecond)
	defer cancel()
	_, err = RunDurableCheck(ctx, "printf effect > result", root, false, DurableExecutionScope{TenantID: "tenant", PersonID: "person", ScratchKey: "watch-one"})
	if err == nil {
		t.Fatal("durable check bypassed host lane")
	}
	if _, err := os.Stat(filepath.Join(root, "result")); !os.IsNotExist(err) {
		t.Fatalf("blocked durable check dispatched: %v", err)
	}
	held()
	_, err = RunDurableCheck(WithExecutionRuntime(context.Background(), runtime), "printf effect > result", root, false, DurableExecutionScope{TenantID: "tenant", PersonID: "person", ScratchKey: "watch-one"})
	if err != nil {
		t.Fatalf("released durable check failed: %v", err)
	}
}

func TestManagedExecutionRechecksPathsAfterWaitingForHost(t *testing.T) {
	withExecSandboxPolicy(t, false, false, false)
	for _, tool := range []string{"write_file", "patch", "terminal"} {
		for _, changed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/changed=%v", tool, changed), func(t *testing.T) {
				runtime := NewExecutionRuntime("path-wait", t.TempDir(), sandbox.PrivateState{}, nil)
				root, outside := t.TempDir(), t.TempDir()
				sub := filepath.Join(root, "target")
				if err := os.Mkdir(sub, 0700); err != nil {
					t.Fatal(err)
				}
				run := "path-waiter"
				t.Cleanup(SetExecutionScope(run, ExecutionScope{TenantID: "tenant", PersonID: "person", RunID: run, WorkspaceRoot: root, AllowedRoots: []string{root}, Runtime: runtime}))
				held, err := runtime.lane(executionLaneKey{"tenant", "person"}).acquire(context.Background(), true)
				if err != nil {
					t.Fatal(err)
				}
				defer held()
				passedScope := make(chan struct{})
				exec := WorkspaceScopeMiddleware()(func(args map[string]interface{}) (string, error) {
					close(passedScope)
					result, err := ManagedExecutionMiddleware()(func(args map[string]interface{}) (kernel.ToolDispatchResult, error) {
						var out string
						var err error
						switch tool {
						case "write_file":
							out, err = NewWriteFileTool().Execute(args)
						case "patch":
							out, err = NewPatchTool().Execute(args)
						case "terminal":
							out, err = NewExecuteCommandTool().Execute(args)
						}
						return kernel.ToolDispatchResult{Output: out}, err
					})(args)
					return result.Output, err
				})
				args := laneTestArgs(run, sub)
				args["_tool_name"] = tool
				args["path"] = filepath.Join(sub, "result")
				args["content"] = "in scope"
				args["patch"] = "*** Begin Patch\n*** Add File: " + filepath.Join(sub, "result") + "\n+in scope\n*** End Patch"
				args["command"], args["sandbox"] = "printf in-scope > result", "host"
				done := make(chan error, 1)
				go func() { _, err := exec(args); done <- err }()
				<-passedScope
				if changed {
					if err := os.Remove(sub); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(outside, sub); err != nil {
						t.Fatal(err)
					}
				}
				held()
				err = <-done
				if _, statErr := os.Stat(filepath.Join(outside, "result")); !os.IsNotExist(statErr) {
					t.Fatalf("post-wait path wrote outside scope: %v (dispatch=%v)", statErr, err)
				}
				if changed && err == nil {
					t.Fatal("post-wait symlink escaped approved roots")
				}
				if !changed {
					if _, statErr := os.Stat(filepath.Join(sub, "result")); err != nil || statErr != nil {
						t.Fatalf("unchanged scope did not execute: %v %v", err, statErr)
					}
				}
			})
		}
	}
}
