package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"selfmind/internal/platform/log"
)

const maintenanceSweepInterval = 30 * time.Second
const maintenancePayloadGrace = time.Minute
const maintenanceMaxAttempts = 5

// Skill-review durable jobs (execution-quality W7) share the maintenance_jobs
// table under their own version namespace; the key is a payload hash, so an
// identical review request enqueued twice is one job. Bounded per pass — the
// review is background learning and must never crowd out post-run analysis.
const (
	SkillReviewJobVersion  = 100
	skillReviewsPerPass    = 2
	skillReviewJobTimeout  = 5 * time.Minute
	skillReviewRetryDelay  = 10 * time.Minute
)

// SkillReviewRunner executes one durable background-review job. Implemented
// by kernel.BackgroundReviewEngine; wired by the gateway runner.
type SkillReviewRunner interface {
	RunReviewFromPayload(ctx context.Context, tenantID, payloadJSON string) (string, error)
}

// StartMaintenanceWorker consumes replayable post-run jobs entirely inside the
// daemon. The immediate finalizer remains the fast path; this worker is the
// reliability path for provider errors, daemon restarts, and lost goroutines.
func (d *Server) StartMaintenanceWorker(ctx context.Context) func() {
	if d == nil || d.Control == nil || (d.PostRunAnalyzer == nil && d.SkillReviewer == nil) {
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
	if d.PostRunAnalyzer == nil {
		d.runSkillReviewPass(ctx)
		return
	}
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
	d.runSkillReviewPass(ctx)
}

// runSkillReviewPass drains a bounded number of durable skill-review jobs
// (W7): CAS claim → execute → complete, with bounded retries on failure. The
// same crash-recovery sweep that resets stale post-run jobs covers these.
func (d *Server) runSkillReviewPass(ctx context.Context) {
	if d.SkillReviewer == nil {
		return
	}
	jobs, err := d.Control.ListRunnableMaintenanceJobs(ctx, SkillReviewJobVersion, skillReviewsPerPass)
	if err != nil {
		return
	}
	for _, job := range jobs {
		if ctx.Err() != nil {
			return
		}
		if job.Attempts >= maintenanceMaxAttempts {
			_ = d.Control.SkipMaintenanceJob(ctx, job.TenantID, job.RunID, job.AnalyzerVersion, "review retry limit reached")
			continue
		}
		claimed, err := d.Control.ClaimMaintenanceJob(ctx, job.TenantID, job.RunID, job.AnalyzerVersion)
		if err != nil || !claimed {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, skillReviewJobTimeout)
		summary, err := d.SkillReviewer.RunReviewFromPayload(callCtx, job.TenantID, job.PayloadJSON)
		cancel()
		if err != nil {
			_ = d.Control.FailMaintenanceJob(ctx, job.TenantID, job.RunID, job.AnalyzerVersion, err.Error(), skillReviewRetryDelay)
			continue
		}
		digest := sha256.Sum256([]byte(summary))
		_ = d.Control.CompleteMaintenanceJob(ctx, job.TenantID, job.RunID, job.AnalyzerVersion, hex.EncodeToString(digest[:8]))
	}
}
