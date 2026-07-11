package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"selfmind/internal/platform/log"
)

const maintenanceSweepInterval = 30 * time.Second
const maintenancePayloadGrace = time.Minute
const maintenanceMaxAttempts = 5

// StartMaintenanceWorker consumes replayable post-run jobs entirely inside the
// daemon. The immediate finalizer remains the fast path; this worker is the
// reliability path for provider errors, daemon restarts, and lost goroutines.
func (d *Server) StartMaintenanceWorker(ctx context.Context) func() {
	if d == nil || d.Control == nil || d.PostRunAnalyzer == nil {
		return func() {}
	}
	// The gateway lock guarantees one daemon. Any job left running at boot lost
	// its owner and can be reclaimed immediately.
	if reset, err := d.Control.ResetStaleMaintenanceJobs(context.Background(), 0); err != nil {
		log.Warn("gateway: reset maintenance jobs at boot failed", "error", err)
	} else if reset > 0 {
		log.Info("gateway: reset maintenance jobs at boot", "count", reset)
	}
	done := make(chan struct{})
	var once sync.Once
	go func() {
		d.runMaintenancePass(ctx)
		ticker := time.NewTicker(maintenanceSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				d.runMaintenancePass(ctx)
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

func (d *Server) runMaintenancePass(ctx context.Context) {
	jobs, err := d.Control.ListRunnableMaintenanceJobs(ctx, postRunAnalyzerVersion, 20)
	if err != nil {
		log.Warn("gateway: maintenance scan failed", "error", err)
		return
	}
	for _, job := range jobs {
		if ctx.Err() != nil {
			return
		}
		if strings.TrimSpace(job.PayloadJSON) == "" {
			// FinishRun creates the durable job before the gateway can attach its
			// replay payload. A sweep racing that small window must not turn a
			// healthy job into a permanent skip.
			if time.Since(job.CreatedAt) < maintenancePayloadGrace {
				continue
			}
			_ = d.Control.SkipMaintenanceJob(ctx, job.TenantID, job.RunID, job.AnalyzerVersion, "terminal run has no replay payload")
			continue
		}
		if job.Attempts >= maintenanceMaxAttempts {
			_ = d.Control.SkipMaintenanceJob(ctx, job.TenantID, job.RunID, job.AnalyzerVersion, "maintenance retry limit reached")
			continue
		}
		var payload postRunJobPayload
		if err := json.Unmarshal([]byte(job.PayloadJSON), &payload); err != nil {
			_ = d.Control.SkipMaintenanceJob(ctx, job.TenantID, job.RunID, job.AnalyzerVersion, "invalid replay payload: "+err.Error())
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, postRunAnalyzerTimeout)
		d.analyzeFinishedRun(callCtx, &payload.Identity, &payload.Task, &payload.Run,
			payload.WorkspaceID, payload.UserInput, payload.Outcome,
			taskAttach{created: payload.AttachCreated, preLabel: payload.AttachPreLabel})
		cancel()
	}
}
