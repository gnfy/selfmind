package httpapi

import (
	"context"
	"selfmind/internal/control"
	"selfmind/internal/tools"
	"selfmind/internal/tools/sandbox"
	"sync/atomic"
	"testing"
	"time"
)

type runtimeScopeRecorder struct{ calls atomic.Int32 }

func (r *runtimeScopeRecorder) Track(int) { r.calls.Add(1) }
func TestPublicWatcherUsesItsServerRuntime(t *testing.T) {
	for _, inherited := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing context runtime", true: "foreign context runtime"}[inherited], func(t *testing.T) {
			d, store, _ := newDetachedRunServer(t, newSlowLLMProvider("unused"))
			first, second := &runtimeScopeRecorder{}, &runtimeScopeRecorder{}
			a := tools.NewExecutionRuntime("A", t.TempDir(), sandbox.PrivateState{}, first)
			b := tools.NewExecutionRuntime("B", t.TempDir(), sandbox.PrivateState{}, second)
			d.ExecutionRuntime = a
			ctx := context.Background()
			owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Me")
			if err != nil {
				t.Fatal(err)
			}
			task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "observe", Channel: "session"})
			if err != nil {
				t.Fatal(err)
			}
			watch, err := store.CreateExternalWatch(ctx, control.ExternalWatch{TenantID: owner.TenantID, PersonID: owner.PersonID, TaskID: task.ID, Channel: "session", CWD: t.TempDir(), Command: "printf PENDING", SuccessPattern: "^SUCCESS$", CommandTimeoutSeconds: 5, TimeoutAt: time.Now().Add(time.Minute)})
			if err != nil {
				t.Fatal(err)
			}
			if inherited {
				ctx = tools.WithExecutionRuntime(ctx, b)
			}
			stop := d.StartExternalWatchWorker(ctx)
			defer stop()
			waitUntil(t, 3*time.Second, func() bool {
				w, _ := store.GetExternalWatch(context.Background(), owner.TenantID, watch.ID)
				// Claiming the attempt precedes process dispatch. Check the actual
				// runtime callback before asserting which runtime was used.
				return w != nil && w.Attempts > 0 && first.calls.Load()+second.calls.Load() > 0
			}, "watch did not execute")
			if first.calls.Load() != 1 || second.calls.Load() != 0 {
				t.Fatalf("watch used wrong runtime: owner=%d foreign=%d", first.calls.Load(), second.calls.Load())
			}
		})
	}
}
