package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// EnsureOptions configures EnsureRunning.
type EnsureOptions struct {
	ConfigPath string
	// Timeout bounds how long to wait for the daemon to answer /health after a
	// start (or while an already-recorded daemon is still coming up). 0 → 10s.
	Timeout time.Duration
}

// EnsureResult reports the resolved daemon and how it was obtained.
type EnsureResult struct {
	URL     string
	Record  StatusRecord
	Started bool // true if EnsureRunning spawned the daemon, false if it was already up
}

// EnsureRunning converges on a single local gateway daemon and returns its URL,
// auto-starting a detached `selfmind gateway run` when none is healthy yet. It
// is the foundation of the daemon-client model: every CLI/TUI client calls this
// so that multiple terminals share ONE owner process (one control.db, one auth
// manager, one worker pool) instead of each building an in-process gateway and
// racing on shared state.
//
// It is concurrency-safe across processes: if two clients start at once, the
// gateway.lock flock in Acquire() guarantees exactly one daemon wins; the loser
// observes ErrAlreadyRunning and both then wait on the winner's /health. A stale
// PID record (process gone) is treated as not-running and a fresh daemon is
// spawned.
func EnsureRunning(ctx context.Context, opts EnsureOptions) (EnsureResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg, err := loadConfigOrDefault(opts.ConfigPath)
	if err != nil {
		return EnsureResult{}, err
	}
	dataDir := resolveDataDir(cfg)
	url := ResolveURL(cfg.Gateway.URL)
	manager := NewManager(dataDir, ResolveAddr(cfg.Gateway.Addr))

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	// Fast path: a live record that already answers /health.
	if rec, ok := manager.RunningRecord(); ok {
		if pingHealth(ctx, url) == nil {
			return EnsureResult{URL: url, Record: rec}, nil
		}
		// Recorded but not answering yet (still starting): wait it out rather
		// than spawning a duplicate that would just lose the flock race.
		if err := waitHealthy(ctx, url, timeout); err == nil {
			rec, _ := manager.RunningRecord()
			return EnsureResult{URL: url, Record: rec}, nil
		}
	}

	// No healthy daemon: start one detached. ErrAlreadyRunning means another
	// client beat us to it — fine, fall through to waiting for that one.
	if _, err := StartDetached(StartOptions{ConfigPath: opts.ConfigPath}); err != nil && !errors.Is(err, ErrAlreadyRunning) {
		return EnsureResult{}, fmt.Errorf("start gateway daemon: %w", err)
	}
	if err := waitHealthy(ctx, url, timeout); err != nil {
		return EnsureResult{}, fmt.Errorf("gateway did not become ready at %s within %s: %w", url, timeout, err)
	}
	rec, _ := manager.RunningRecord()
	return EnsureResult{URL: url, Record: rec, Started: true}, nil
}

// waitHealthy polls /health until it returns 200, the deadline passes, or ctx is
// cancelled. The poll interval is short so startup latency is barely noticeable.
func waitHealthy(ctx context.Context, url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if lastErr = pingHealth(ctx, url); lastErr == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return lastErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// pingHealth does one short GET /health. /health needs no auth, but we attach
// the token anyway so a future authenticated health check keeps working.
func pingHealth(ctx context.Context, url string) error {
	reqCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url+"/health", nil)
	if err != nil {
		return err
	}
	attachAuth(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned %s", resp.Status)
	}
	return nil
}
