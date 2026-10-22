package router

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

// countingProvider answers every request at once and counts the requests, so
// a test can tell which worker's agent actually ran a turn.
type countingProvider struct {
	calls atomic.Int32
}

func (p *countingProvider) ChatCompletion(context.Context, []llm.Message) (string, error) {
	p.calls.Add(1)
	return "done", nil
}

func (p *countingProvider) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	p.calls.Add(1)
	return &llm.ChatResponse{Content: "done", FinishReason: "stop"}, nil
}

func (p *countingProvider) StreamChat(context.Context, llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.calls.Add(1)
	ch := make(chan llm.StreamEvent, 2)
	ch <- llm.StreamEvent{Content: "done"}
	ch <- llm.StreamEvent{FinishReason: "stop"}
	close(ch)
	return ch, nil
}

func newLeaseTestGateway(t *testing.T, workers int) (*Gateway, []*countingProvider) {
	t.Helper()
	providers := make([]*countingProvider, workers)
	agents := make([]*kernel.Agent, workers)
	for i := range agents {
		providers[i] = &countingProvider{}
		agents[i] = kernel.NewAgent(memory.NewMemoryManager(nil), noToolBackend{}, providers[i], "helpful", 3, 1, nil)
	}
	g := NewGateway(agents[0], nil)
	g.EnableWorkerPool(agents[1:])
	return g, providers
}

// readOnlyTurn keeps two concurrent test turns off the shared write lock, the
// way a turn that cannot write the workspace runs beside another.
func readOnlyTurn() context.Context {
	strategy := kernel.DefaultTaskStrategy()
	strategy.ToolMode = kernel.ToolModeNone
	return kernel.WithTaskStrategy(context.Background(), strategy)
}

func drainTurn(t *testing.T, resp *HandleResponse, err error) error {
	t.Helper()
	if err != nil {
		return err
	}
	var streamErr error
	for event := range resp.Stream {
		if event.Err != nil {
			streamErr = event.Err
		}
	}
	return streamErr
}

// Two turns in flight together lease two different workers, and the worker
// the hook names is the one whose agent then runs the turn.
func TestWorkerLeaseHookNamesTheWorkerThatRunsEachConcurrentTurn(t *testing.T) {
	g, providers := newLeaseTestGateway(t, 2)
	var mu sync.Mutex
	leased := map[int]int{}
	released := map[int]int{}
	bothLeased := make(chan struct{})
	g.SetWorkerLeaseHook(func(ctx context.Context, worker int) (func(), error) {
		mu.Lock()
		leased[worker]++
		if len(leased) == 2 {
			close(bothLeased)
		}
		mu.Unlock()
		// Hold every lease until both turns have one, so the turns provably
		// overlap on the pool.
		select {
		case <-bothLeased:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
			return nil, errors.New("the other turn never leased a worker")
		}
		return func() {
			mu.Lock()
			released[worker]++
			mu.Unlock()
		}, nil
	})

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := g.RunAgent(readOnlyTurn(), "person", "session", "summarize the notes")
			errs[i] = drainTurn(t, resp, err)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
	}
	if leased[0] != 1 || leased[1] != 1 {
		t.Fatalf("leases by worker = %v, want one turn on each of two workers", leased)
	}
	if released[0] != 1 || released[1] != 1 {
		t.Fatalf("releases by worker = %v, want every lease released", released)
	}
	for worker, provider := range providers {
		if provider.calls.Load() == 0 {
			t.Fatalf("worker %d was leased but its agent never ran the turn", worker)
		}
	}
}

// A hook error ends the turn before the agent starts, like a cancellation
// while waiting for a worker, and the worker returns to the pool.
func TestWorkerLeaseHookErrorEndsTheTurnWithoutStartingTheAgent(t *testing.T) {
	for _, workers := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("%d workers", workers), func(t *testing.T) {
			g, providers := newLeaseTestGateway(t, workers)
			refuse := errors.New("worker held by test")
			var refused atomic.Bool
			var mu sync.Mutex
			leased := 0
			allLeased := make(chan struct{})
			g.SetWorkerLeaseHook(func(ctx context.Context, worker int) (func(), error) {
				if refused.CompareAndSwap(false, true) {
					return nil, refuse
				}
				// Every later turn holds its worker until each has one. A worker
				// the refused turn kept would leave one turn without a worker,
				// so the others could never all meet.
				mu.Lock()
				leased++
				if leased == workers {
					close(allLeased)
				}
				mu.Unlock()
				select {
				case <-allLeased:
					return func() {}, nil
				case <-time.After(3 * time.Second):
					return nil, errors.New("not every worker came back to the pool")
				}
			})
			resp, err := g.RunAgent(readOnlyTurn(), "person", "session", "summarize the notes")
			if got := drainTurn(t, resp, err); !errors.Is(got, refuse) {
				t.Fatalf("refused turn error = %v, want the hook's error", got)
			}
			for worker, provider := range providers {
				if calls := provider.calls.Load(); calls != 0 {
					t.Fatalf("worker %d agent ran %d provider calls for a refused turn", worker, calls)
				}
			}
			if g.agents != nil && len(g.agents) != workers {
				t.Fatalf("idle workers after a refused lease = %d, want all %d back", len(g.agents), workers)
			}
			// One turn per worker, all in flight together.
			var wg sync.WaitGroup
			errs := make([]error, workers)
			for i := range errs {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					resp, err := g.RunAgent(readOnlyTurn(), "person", "session", "summarize the notes")
					errs[i] = drainTurn(t, resp, err)
				}(i)
			}
			wg.Wait()
			for i, err := range errs {
				if err != nil {
					t.Fatalf("turn %d after a refused lease: %v", i, err)
				}
			}
		})
	}
}

// The single-worker gateway, which runs without a pool, still reports its one
// worker, so a harness can attribute every Run regardless of the pool size.
func TestWorkerLeaseHookCoversTheSingleWorkerGateway(t *testing.T) {
	g, providers := newLeaseTestGateway(t, 1)
	var leasedWorker atomic.Int32
	leasedWorker.Store(-1)
	var releasedAfterRun atomic.Bool
	g.SetWorkerLeaseHook(func(ctx context.Context, worker int) (func(), error) {
		leasedWorker.Store(int32(worker))
		return func() { releasedAfterRun.Store(providers[0].calls.Load() > 0) }, nil
	})
	resp, err := g.RunAgent(readOnlyTurn(), "person", "session", "summarize the notes")
	if got := drainTurn(t, resp, err); got != nil {
		t.Fatal(got)
	}
	if leasedWorker.Load() != 0 {
		t.Fatalf("single worker lease = %d, want worker 0", leasedWorker.Load())
	}
	if !releasedAfterRun.Load() {
		t.Fatal("the lease was not released after the agent ran")
	}
}
