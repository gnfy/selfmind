package httpapi

import (
	"context"
	"sync"
	"time"

	"selfmind/internal/platform/log"
)

// MemoryConsolidator runs one background self-organization pass over one
// person's memory partition. Implementations live in internal/app so the
// gateway stays provider- and storage-agnostic; RunOnce must be safe to call
// repeatedly (cluster-level checkpointing) and must never touch pinned or
// user-confirmed memory.
type MemoryConsolidator interface {
	RunOnce(ctx context.Context, personID string) error
	Interval() time.Duration
	PauseWhileRunActive() bool
	Mode() string
}

// StartMemoryGovernance starts the periodic consolidation loop. Foreground
// work always wins: a tick is skipped entirely while any run is active. Each
// person partition is processed sequentially with a bounded per-person budget
// so one huge partition cannot monopolize the daemon.
func (d *Server) StartMemoryGovernance(ctx context.Context) func() {
	if d == nil || d.MemoryConsolidator == nil || d.Control == nil {
		return func() {}
	}
	interval := d.MemoryConsolidator.Interval()
	done := make(chan struct{})
	var once sync.Once
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				d.runMemoryGovernancePass(ctx)
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

func (d *Server) runMemoryGovernancePass(ctx context.Context) {
	pauseForForeground := d.MemoryConsolidator.PauseWhileRunActive()
	if pauseForForeground && len(d.coordinator().activeRunIDs()) > 0 {
		log.Info("memory governance: tick skipped, foreground run active")
		return
	}
	persons, err := d.Control.ListPersonIDs(ctx)
	if err != nil {
		log.Warn("memory governance: person listing failed", "error", err)
		return
	}
	for _, person := range persons {
		if ctx.Err() != nil {
			return
		}
		// Re-check before each partition: a run that started mid-pass pauses
		// the remainder; the next tick resumes from the judgement checkpoints.
		if pauseForForeground && len(d.coordinator().activeRunIDs()) > 0 {
			log.Info("memory governance: pass paused, foreground run started")
			return
		}
		passCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		if err := d.MemoryConsolidator.RunOnce(passCtx, person); err != nil {
			log.Warn("memory governance: pass failed", "person", person, "error", err)
		}
		cancel()
	}
}
