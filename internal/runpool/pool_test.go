package runpool

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestPoolBoundsConcurrency: with N workers, no more than N jobs run at once.
func TestPoolBoundsConcurrency(t *testing.T) {
	const workers = 3
	p := New(workers)
	var live, maxLive int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		key := "ws-" + string(rune('a'+i)) // distinct keys → only the worker bound applies
		go func(key string) {
			defer wg.Done()
			_ = p.Run(context.Background(), key, func() error {
				n := atomic.AddInt32(&live, 1)
				for {
					m := atomic.LoadInt32(&maxLive)
					if n <= m || atomic.CompareAndSwapInt32(&maxLive, m, n) {
						break
					}
				}
				time.Sleep(20 * time.Millisecond)
				atomic.AddInt32(&live, -1)
				return nil
			})
		}(key)
	}
	wg.Wait()
	if maxLive > workers {
		t.Fatalf("max concurrent = %d, want <= %d", maxLive, workers)
	}
}

// TestPoolSerializesSameKey: jobs sharing a key never run concurrently.
func TestPoolSerializesSameKey(t *testing.T) {
	p := New(8) // plenty of slots; serialization must come from the key
	var live, maxLive int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = p.Run(context.Background(), "same-workspace", func() error {
				n := atomic.AddInt32(&live, 1)
				for {
					m := atomic.LoadInt32(&maxLive)
					if n <= m || atomic.CompareAndSwapInt32(&maxLive, m, n) {
						break
					}
				}
				time.Sleep(5 * time.Millisecond)
				atomic.AddInt32(&live, -1)
				return nil
			})
		}()
	}
	wg.Wait()
	if maxLive != 1 {
		t.Fatalf("same-key max concurrent = %d, want 1", maxLive)
	}
}

// TestPoolDifferentKeysRunInParallel: two distinct keys must overlap (would
// deadlock/time out if serialized).
func TestPoolDifferentKeysRunInParallel(t *testing.T) {
	p := New(2)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	run := func(key string) {
		_ = p.Run(context.Background(), key, func() error {
			started <- struct{}{}
			<-release
			return nil
		})
	}
	go run("ws-1")
	go run("ws-2")

	deadline := time.After(time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-deadline:
			t.Fatal("distinct-key jobs did not run in parallel (still serialized)")
		}
	}
	close(release)
}

// TestPoolContextCancelWhileWaiting: a job blocked on a busy key returns
// ctx.Err() and never runs fn when its context is cancelled.
func TestPoolContextCancelWhileWaiting(t *testing.T) {
	p := New(4)
	holding := make(chan struct{})
	releaseHolder := make(chan struct{})
	go func() {
		_ = p.Run(context.Background(), "k", func() error {
			close(holding)
			<-releaseHolder
			return nil
		})
	}()
	<-holding // ensure the key is held

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	var ran bool
	err := p.Run(ctx, "k", func() error { ran = true; return nil })
	if err == nil {
		t.Fatal("expected ctx error for a cancelled waiter")
	}
	if ran {
		t.Fatal("fn must not run when the wait was cancelled")
	}
	close(releaseHolder)
}

func TestPoolWorkersClampedToOne(t *testing.T) {
	if got := New(0).Workers(); got != 1 {
		t.Fatalf("New(0).Workers() = %d, want 1", got)
	}
}

func TestPoolReportsResourceWait(t *testing.T) {
	p := New(2)
	holding := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = p.Run(context.Background(), "workspace", func() error {
			close(holding)
			<-release
			return nil
		})
	}()
	<-holding

	ctx, cancel := context.WithCancel(context.Background())
	states := make(chan State, 2)
	done := make(chan error, 1)
	go func() {
		done <- p.RunObserved(ctx, "workspace", func(state State) { states <- state }, func() error { return nil })
	}()
	select {
	case state := <-states:
		if state != StateWaitingResource {
			t.Fatalf("first state = %q, want %q", state, StateWaitingResource)
		}
	case <-time.After(time.Second):
		t.Fatal("missing waiting_resource state")
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled resource waiter returned nil")
	}
	close(release)
}

func TestPoolReportsWorkerWait(t *testing.T) {
	p := New(1)
	holding := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = p.Run(context.Background(), "", func() error {
			close(holding)
			<-release
			return nil
		})
	}()
	<-holding

	ctx, cancel := context.WithCancel(context.Background())
	states := make(chan State, 2)
	done := make(chan error, 1)
	go func() {
		done <- p.RunObserved(ctx, "", func(state State) { states <- state }, func() error { return nil })
	}()
	select {
	case state := <-states:
		if state != StateWaitingWorker {
			t.Fatalf("first state = %q, want %q", state, StateWaitingWorker)
		}
	case <-time.After(time.Second):
		t.Fatal("missing waiting_worker state")
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled worker waiter returned nil")
	}
	close(release)
}

func TestPoolOverlappingPathsSerialize(t *testing.T) {
	p := New(2)
	root := t.TempDir()
	holding := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = p.RunObservedPaths(context.Background(), []string{root}, nil, func() error {
			close(holding)
			<-release
			return nil
		})
	}()
	<-holding

	states := make(chan State, 2)
	done := make(chan error, 1)
	go func() {
		done <- p.RunObservedPaths(context.Background(), []string{filepath.Join(root, "nested")}, func(state State) {
			states <- state
		}, func() error { return nil })
	}()
	select {
	case state := <-states:
		if state != StateWaitingResource {
			t.Fatalf("state = %q, want %q", state, StateWaitingResource)
		}
	case <-time.After(time.Second):
		t.Fatal("nested path did not wait for ancestor claim")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPoolDisjointPathSetsRunInParallel(t *testing.T) {
	p := New(2)
	first, second := t.TempDir(), t.TempDir()
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	for _, roots := range [][]string{{first}, {second}} {
		roots := roots
		go func() {
			_ = p.RunObservedPaths(context.Background(), roots, nil, func() error {
				started <- struct{}{}
				<-release
				return nil
			})
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("disjoint path claims did not run in parallel")
		}
	}
	close(release)
}

func TestPoolPathWaitHonorsCancellation(t *testing.T) {
	p := New(2)
	root := t.TempDir()
	holding := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = p.RunObservedPaths(context.Background(), []string{root}, nil, func() error {
			close(holding)
			<-release
			return nil
		})
	}()
	<-holding
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ran := false
	err := p.RunObservedPaths(ctx, []string{filepath.Join(root, "child")}, nil, func() error {
		ran = true
		return nil
	})
	close(release)
	if err == nil || ran {
		t.Fatalf("cancelled waiter err=%v ran=%v", err, ran)
	}
}
