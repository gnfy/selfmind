package httpapi

import (
	"context"
	"sync"

	"selfmind/internal/platform/log"
)

// Background services — post-run maintenance and memory governance — used to be
// started once, inline, at gateway startup: ready, and they run; not ready, and
// a warning was logged and nothing ever reconsidered. Readiness recovering later
// therefore left them parked for the life of the daemon. Observed 2026-09-05 on
// a daemon whose foreground routes were verified while background_verified_at
// stayed at its zero value.
//
// The foreground already had a recovery path (drainQueuedWhenReady runs when a
// pending model change is cancelled); this gives the background one owner with
// the same property, so every readiness transition has somewhere to land.
type backgroundServices struct {
	mu      sync.Mutex
	started bool
	stops   []func()
}

// EnsureBackgroundServices starts maintenance and memory governance if the
// background route is ready and they are not already running. It is idempotent
// and safe to call from any readiness transition.
//
// Losing readiness does not stop what is already running: those workers survive
// provider failure on their own and stopping mid-cycle would abandon durable
// evidence for no gain. This starts; shutdown stops.
func (d *Server) EnsureBackgroundServices(ctx context.Context) {
	if d == nil {
		return
	}
	d.background.mu.Lock()
	defer d.background.mu.Unlock()
	if d.background.started {
		return
	}
	if !d.backgroundReadyForWork() {
		return
	}
	d.background.stops = append(d.background.stops,
		d.StartMaintenanceWorker(ctx),
		d.StartMemoryGovernance(ctx),
	)
	d.background.started = true
	log.Info("gateway: background maintenance and memory governance started")
}

// StopBackgroundServices releases whatever EnsureBackgroundServices started.
func (d *Server) StopBackgroundServices() {
	if d == nil {
		return
	}
	d.background.mu.Lock()
	stops := d.background.stops
	d.background.stops = nil
	d.background.started = false
	d.background.mu.Unlock()
	for _, stop := range stops {
		if stop != nil {
			stop()
		}
	}
}
