// Package runpool provides the scheduling primitive for the agent worker pool
// (docs/worker-pool-design.md, W1b). It bounds total concurrency to N workers
// and serializes jobs that share a non-empty key (e.g. a workspace id) so the
// same project is never written by two turns at once, while different keys run
// in parallel. It is deliberately dependency-free so it can be unit/race-tested
// in isolation before being wired into the live run path.
package runpool

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
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
	sem           chan struct{} // capacity = worker count (total concurrency bound)
	mu            sync.Mutex
	keyLocks      map[string]*keyLock
	pathClaims    map[uint64][]string
	pathChanged   chan struct{}
	nextPathClaim uint64
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
	return &Pool{
		sem: make(chan struct{}, workers), keyLocks: make(map[string]*keyLock),
		pathClaims: make(map[uint64][]string), pathChanged: make(chan struct{}),
	}
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
	return p.runAfterResource(ctx, waited, notify, fn)
}

// RunObservedPaths serializes writers whose canonical filesystem roots are
// equal OR overlap by ancestry. A simple per-string key is insufficient for
// --add-dir: /repo and /repo/packages/shared are different strings but the
// first writer can still modify the second tree. All roots are acquired as one
// claim before a worker slot, so multi-root runs cannot deadlock one another.
func (p *Pool) RunObservedPaths(ctx context.Context, paths []string, observe StateObserver, fn func() error) error {
	paths = normalizePaths(paths)
	waited := false
	notify := func(state State) {
		if state == StateWaitingResource || state == StateWaitingWorker {
			waited = true
		}
		if observe != nil {
			observe(state)
		}
	}
	claimID, err := p.lockPaths(ctx, paths, notify)
	if err != nil {
		return err
	}
	if claimID != 0 {
		defer p.unlockPaths(claimID)
	}
	return p.runAfterResource(ctx, waited, notify, fn)
}

func (p *Pool) runAfterResource(ctx context.Context, waited bool, notify StateObserver, fn func() error) error {
	select {
	case p.sem <- struct{}{}:
		// Acquired without waiting.
	default:
		waited = true
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

func (p *Pool) lockPaths(ctx context.Context, paths []string, observe StateObserver) (uint64, error) {
	if len(paths) == 0 {
		return 0, nil
	}
	notified := false
	for {
		p.mu.Lock()
		if !p.pathsConflictLocked(paths) {
			p.nextPathClaim++
			id := p.nextPathClaim
			p.pathClaims[id] = append([]string{}, paths...)
			p.mu.Unlock()
			return id, nil
		}
		changed := p.pathChanged
		p.mu.Unlock()
		if !notified && observe != nil {
			observe(StateWaitingResource)
			notified = true
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}

func (p *Pool) pathsConflictLocked(candidate []string) bool {
	for _, held := range p.pathClaims {
		for _, a := range candidate {
			for _, b := range held {
				if pathsOverlap(a, b) {
					return true
				}
			}
		}
	}
	return false
}

func (p *Pool) unlockPaths(id uint64) {
	if id == 0 {
		return
	}
	p.mu.Lock()
	delete(p.pathClaims, id)
	close(p.pathChanged)
	p.pathChanged = make(chan struct{})
	p.mu.Unlock()
}

func normalizePaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if absolute, err := filepath.Abs(path); err == nil {
			path = absolute
		}
		path = filepath.Clean(path)
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

func pathsOverlap(a, b string) bool {
	return pathWithin(a, b) || pathWithin(b, a)
}

func pathWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
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
