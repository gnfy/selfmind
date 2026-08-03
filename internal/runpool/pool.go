// Package runpool provides the scheduling primitive for the agent worker pool
// (docs/worker-pool-design.md, W1b). It bounds total concurrency to N workers
// and serializes jobs that share a non-empty key (e.g. a workspace id) so the
// same project is never written by two turns at once, while different keys run
// in parallel. It is deliberately dependency-free so it can be unit/race-tested
// in isolation before being wired into the live run path.
package runpool

import (
	"context"
	"sync"
)

// State describes where a job is in pool admission.
type State string

const (
	StateWaitingResource State = "waiting_resource"
	StateWaitingWorker   State = "waiting_worker"
	StateRunning         State = "running"
)

// StateObserver receives edge-triggered admission state changes.
type StateObserver func(State)

// Pool bounds concurrent jobs and provides per-key serialization.
type Pool struct {
	sem      chan struct{} // capacity = worker count (total concurrency bound)
	mu       sync.Mutex
	keyLocks map[string]*keyLock
}

// keyLock is a 1-capacity channel used as a mutex, ref-counted so unused keys
// are garbage-collected from the map.
type keyLock struct {
	ch   chan struct{}
	refs int
}

// New creates a pool with the given worker count (clamped to >= 1).
func New(workers int) *Pool {
	if workers < 1 {
		workers = 1
	}
	return &Pool{sem: make(chan struct{}, workers), keyLocks: make(map[string]*keyLock)}
}

// Workers returns the configured worker count.
func (p *Pool) Workers() int { return cap(p.sem) }

// Run serializes by key (when non-empty), bounds total concurrency to the
// worker count, runs fn, and returns its error. If ctx is cancelled while
// waiting for either the key lock or a worker slot, fn is not run and ctx.Err()
// is returned.
//
// Acquisition order is key-lock then worker-slot: a job waiting on a busy key
// does not hold a worker slot, so it cannot starve other keys.
func (p *Pool) Run(ctx context.Context, key string, fn func() error) error {
	return p.RunObserved(ctx, key, nil, fn)
}

// RunObserved is Run with best-effort admission-state notifications.
func (p *Pool) RunObserved(ctx context.Context, key string, observe StateObserver, fn func() error) error {
	waited := false
	notify := func(state State) {
		if state == StateWaitingResource || state == StateWaitingWorker {
			waited = true
		}
		if observe != nil {
			observe(state)
		}
	}
	if key != "" {
		if err := p.lockKey(ctx, key, notify); err != nil {
			return err
		}
		defer p.unlockKey(key)
	}
	select {
	case p.sem <- struct{}{}:
		// Acquired without waiting.
	default:
		notify(StateWaitingWorker)
		select {
		case p.sem <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	defer func() { <-p.sem }()
	if waited {
		notify(StateRunning)
	}
	return fn()
}

func (p *Pool) lockKey(ctx context.Context, key string, observe StateObserver) error {
	p.mu.Lock()
	kl := p.keyLocks[key]
	if kl == nil {
		kl = &keyLock{ch: make(chan struct{}, 1)}
		p.keyLocks[key] = kl
	}
	kl.refs++
	p.mu.Unlock()

	select {
	case kl.ch <- struct{}{}:
		return nil
	default:
	}
	if observe != nil {
		observe(StateWaitingResource)
	}
	select {
	case kl.ch <- struct{}{}: // acquired the per-key mutex
		return nil
	case <-ctx.Done():
		p.releaseRef(key)
		return ctx.Err()
	}
}

func (p *Pool) unlockKey(key string) {
	p.mu.Lock()
	if kl := p.keyLocks[key]; kl != nil {
		<-kl.ch // release the per-key mutex (drains our own token)
		kl.refs--
		if kl.refs == 0 {
			delete(p.keyLocks, key)
		}
	}
	p.mu.Unlock()
}

// releaseRef drops a waiter's reference after a cancelled wait.
func (p *Pool) releaseRef(key string) {
	p.mu.Lock()
	if kl := p.keyLocks[key]; kl != nil {
		kl.refs--
		if kl.refs == 0 {
			delete(p.keyLocks, key)
		}
	}
	p.mu.Unlock()
}
