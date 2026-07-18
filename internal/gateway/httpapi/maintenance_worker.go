package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/platform/log"
)

const maintenanceSweepInterval = 30 * time.Second
const maintenancePayloadGrace = time.Minute
const maintenanceMaxAttempts = 5

const (
	defaultMaintenanceDebounce = 5 * time.Minute
	defaultMaintenanceMaxWait  = 15 * time.Minute
	defaultMaintenanceBatchMax = 10
	maintenanceScanLimit       = 500
)

// PostRunMaintenanceOptions bounds how long completed-run evidence can wait
// before background governance. Zero values use product defaults; callers can
// use a negative duration in focused tests to make a pass immediately due.
type PostRunMaintenanceOptions struct {
	Debounce     time.Duration
	MaxWait      time.Duration
	BatchMaxRuns int
	// LLMTimeout bounds one analyzer provider call. Zero uses the default;
	// it exists because a too-tight bound silently converts a slow cheap-role
	// provider into five deadline-exceeded retries and a skipped job.
	LLMTimeout time.Duration
}

func (o PostRunMaintenanceOptions) normalized() PostRunMaintenanceOptions {
	if o.Debounce == 0 {
		o.Debounce = defaultMaintenanceDebounce
	}
	if o.MaxWait == 0 {
		o.MaxWait = defaultMaintenanceMaxWait
	}
	if o.MaxWait > 0 && o.Debounce > o.MaxWait {
		o.MaxWait = o.Debounce
	}
	if o.BatchMaxRuns <= 0 {
		o.BatchMaxRuns = defaultMaintenanceBatchMax
	}
	if o.BatchMaxRuns > 50 {
		o.BatchMaxRuns = 50
	}
	if o.LLMTimeout <= 0 {
		o.LLMTimeout = defaultPostRunAnalyzerTimeout
	}
	return o
}

// analyzerTimeout is the per-call provider bound from the normalized options.
func (d *Server) analyzerTimeout() time.Duration {
	return d.PostRunMaintenance.normalized().LLMTimeout
}

// Skill-review durable jobs (execution-quality W7) share the maintenance_jobs
// table under their own version namespace; the key is a payload hash, so an
// identical review request enqueued twice is one job. Bounded per pass — the
// review is background learning and must never crowd out post-run analysis.
const (
	SkillReviewJobVersion = 100
	skillReviewsPerPass   = 2
	skillReviewJobTimeout = 5 * time.Minute
	skillReviewRetryDelay = 10 * time.Minute
)

// SkillReviewRunner executes one durable background-review job. Implemented
// by kernel.BackgroundReviewEngine; wired by the gateway runner.
type SkillReviewRunner interface {
	RunReviewFromPayload(ctx context.Context, tenantID, payloadJSON string) (string, error)
}

// StartMaintenanceWorker is the only post-run governance executor. Run
// finalization persists evidence and returns; this worker debounces nearby
// evidence by person/workspace, batches the model decision, and retains the
// durable proposal/replay path for provider errors and daemon restarts.
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
	if reset, err := d.Control.ResetLegacyBlockedMaintenanceJobs(context.Background()); err != nil {
		log.Warn("gateway: reset provider-blocked maintenance jobs at boot failed", "error", err)
	} else if reset > 0 {
		log.Info("gateway: retrying legacy provider-blocked maintenance jobs after restart", "count", reset)
	}
	if pruned, err := d.Control.PruneMaintenanceAttempts(context.Background(), 0); err != nil {
		log.Warn("gateway: prune maintenance attempt history failed", "error", err)
	} else if pruned > 0 {
		log.Info("gateway: pruned maintenance attempt history", "count", pruned)
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
	d.runMaintenancePassAt(ctx, time.Now())
}

type queuedPostRunMaintenance struct {
	job      control.MaintenanceJob
	payload  postRunJobPayload
	prepared *preparedPostRunAnalysis
}

type postRunMaintenanceGroup struct {
	items  []*queuedPostRunMaintenance
	oldest time.Time
	latest time.Time
}

func (d *Server) runMaintenancePassAt(ctx context.Context, now time.Time) {
	if requeued, err := d.Control.RequeueDueProviderRouteProbes(ctx, now); err != nil {
		log.Warn("gateway: quota probe scheduling failed", "error", err)
	} else if requeued > 0 {
		log.Info("gateway: scheduled provider quota probe", "routes", requeued)
	}
	if d.PostRunAnalyzer == nil {
		d.runSkillReviewPass(ctx)
		return
	}
	jobs, err := d.Control.ListRunnableMaintenanceJobs(ctx, postRunAnalyzerVersion, maintenanceScanLimit)
	if err != nil {
		log.Warn("gateway: maintenance scan failed", "error", err)
		return
	}
	groups := make(map[string]*postRunMaintenanceGroup)
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
		if payload.WorkspaceID == "" {
			payload.WorkspaceID = payload.Run.WorkspaceID
		}
		attach := taskAttach{created: payload.AttachCreated, preLabel: payload.AttachPreLabel}
		// A frozen proposal means model work already completed before a crash.
		// Apply it immediately; making it wait for another debounce window would
		// only delay recovery and cannot save another model call.
		if strings.TrimSpace(job.ProposalJSON) != "" {
			callCtx, cancel := context.WithTimeout(ctx, d.analyzerTimeout())
			d.analyzeFinishedRun(callCtx, &payload.Identity, &payload.Task, &payload.Run,
				payload.WorkspaceID, payload.UserInput, payload.Outcome, attach)
			cancel()
			continue
		}
		prepared := d.preparePostRunAnalysis(ctx, &payload.Identity, &payload.Task, &payload.Run,
			payload.WorkspaceID, payload.UserInput, payload.Outcome, attach)
		if prepared == nil {
			continue
		}
		key := strings.Join([]string{job.TenantID, payload.Identity.PersonID, payload.WorkspaceID}, "\x00")
		group := groups[key]
		if group == nil {
			group = &postRunMaintenanceGroup{oldest: job.CreatedAt, latest: job.UpdatedAt}
			groups[key] = group
		}
		if job.CreatedAt.Before(group.oldest) {
			group.oldest = job.CreatedAt
		}
		if job.UpdatedAt.After(group.latest) {
			group.latest = job.UpdatedAt
		}
		group.items = append(group.items, &queuedPostRunMaintenance{job: job, payload: payload, prepared: prepared})
	}
	policy := d.PostRunMaintenance.normalized()
	for _, group := range groups {
		if ctx.Err() != nil {
			return
		}
		ready := len(group.items) >= policy.BatchMaxRuns ||
			policy.Debounce < 0 || now.Sub(group.latest) >= policy.Debounce ||
			policy.MaxWait < 0 || now.Sub(group.oldest) >= policy.MaxWait
		if !ready {
			continue
		}
		for len(group.items) > 0 {
			n := policy.BatchMaxRuns
			if n > len(group.items) {
				n = len(group.items)
			}
			d.runPostRunMaintenanceBatch(ctx, group.items[:n])
			group.items = group.items[n:]
			// A quiet-window pass may drain all accumulated evidence. A batch
			// size trigger processes only the full batch and leaves the fresh tail
			// for the next debounce window.
			if len(group.items) < policy.BatchMaxRuns && now.Sub(group.latest) < policy.Debounce && now.Sub(group.oldest) < policy.MaxWait {
				break
			}
		}
	}
	d.runSkillReviewPass(ctx)
}

func (d *Server) runPostRunMaintenanceBatch(ctx context.Context, items []*queuedPostRunMaintenance) {
	if len(items) == 0 {
		return
	}
	batchAnalyzer, supportsBatch := d.PostRunAnalyzer.(PostRunBatchAnalyzer)
	if !supportsBatch {
		for _, item := range items {
			callCtx, cancel := context.WithTimeout(ctx, d.analyzerTimeout())
			d.analyzeFinishedRun(callCtx, &item.payload.Identity, &item.payload.Task, &item.payload.Run,
				item.payload.WorkspaceID, item.payload.UserInput, item.payload.Outcome,
				taskAttach{created: item.payload.AttachCreated, preLabel: item.payload.AttachPreLabel})
			cancel()
		}
		return
	}

	claimed := make([]*queuedPostRunMaintenance, 0, len(items))
	requests := make([]PostRunAnalysisRequest, 0, len(items))
	for _, item := range items {
		ok, err := d.Control.ClaimMaintenanceJob(ctx, item.job.TenantID, item.job.RunID, postRunAnalyzerVersion)
		if err != nil {
			log.Warn("gateway: maintenance batch claim failed", "run", item.job.RunID, "error", err)
			continue
		}
		if !ok {
			continue
		}
		claimed = append(claimed, item)
		requests = append(requests, item.prepared.request)
	}
	if len(claimed) == 0 {
		return
	}
	d.processClaimedPostRunBatch(ctx, batchAnalyzer, claimed, requests)
}

// processClaimedPostRunBatch degrades a failed aggregate request by bisecting
// it until the bad/truncated response is isolated. A reasoning-heavy model can
// occasionally exhaust its output budget on a large JSON batch; one such run
// must not discard otherwise valid maintenance work for the whole bucket.
func (d *Server) processClaimedPostRunBatch(ctx context.Context, batchAnalyzer PostRunBatchAnalyzer, claimed []*queuedPostRunMaintenance, requests []PostRunAnalysisRequest) {
	if len(claimed) == 0 || len(claimed) != len(requests) {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, d.postRunMaintenanceBatchTimeout(len(claimed)))
	results, err := batchAnalyzer.AnalyzeBatch(callCtx, requests)
	cancel()
	if err != nil {
		if len(claimed) > 1 && llm.IsRetryableError(err) {
			mid := len(claimed) / 2
			d.processClaimedPostRunBatch(ctx, batchAnalyzer, claimed[:mid], requests[:mid])
			d.processClaimedPostRunBatch(ctx, batchAnalyzer, claimed[mid:], requests[mid:])
			return
		}
		for _, item := range claimed {
			d.failClaimedPostRun(ctx, item.prepared, err)
		}
		return
	}
	missingClaimed := make([]*queuedPostRunMaintenance, 0)
	missingRequests := make([]PostRunAnalysisRequest, 0)
	for _, item := range claimed {
		analysis, ok := results[item.job.RunID]
		if !ok {
			missingClaimed = append(missingClaimed, item)
			for _, req := range requests {
				if req.RunID == item.job.RunID {
					missingRequests = append(missingRequests, req)
					break
				}
			}
			continue
		}
		if !d.saveMaintenanceProposal(ctx, item.prepared, analysis) {
			continue
		}
		d.applyClaimedPostRun(ctx, item.prepared, analysis)
	}
	if len(missingClaimed) == 0 {
		return
	}
	if len(missingClaimed) == 1 {
		d.failClaimedPostRun(ctx, missingClaimed[0].prepared,
			fmt.Errorf("maintenance batch response omitted run %s", missingClaimed[0].job.RunID))
		return
	}
	if len(missingClaimed) < len(claimed) {
		d.processClaimedPostRunBatch(ctx, batchAnalyzer, missingClaimed, missingRequests)
		return
	}
	// The provider omitted every run. Repeating the same aggregate request is
	// unlikely to help, so split it just like a truncated response.
	mid := len(missingClaimed) / 2
	d.processClaimedPostRunBatch(ctx, batchAnalyzer, missingClaimed[:mid], missingRequests[:mid])
	d.processClaimedPostRunBatch(ctx, batchAnalyzer, missingClaimed[mid:], missingRequests[mid:])
}

func (d *Server) postRunMaintenanceBatchTimeout(size int) time.Duration {
	if size < 1 {
		size = 1
	}
	base := d.analyzerTimeout()
	timeout := base + time.Duration(size-1)*5*time.Second
	// The batch bound grows with size but never exceeds twice the per-call
	// bound: past that point bisection is cheaper than waiting.
	if cap := 2 * base; timeout > cap {
		return cap
	}
	return timeout
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
			if llm.IsRetryableError(err) {
				_ = d.Control.FailMaintenanceJob(ctx, job.TenantID, job.RunID, job.AnalyzerVersion, err.Error(), skillReviewRetryDelay)
			} else {
				info, _ := llm.ProviderErrorInfo(err)
				_, _ = d.Control.BlockMaintenanceJobForRoute(ctx, job.TenantID, job.RunID, job.AnalyzerVersion, info.RouteID, err.Error())
			}
			continue
		}
		digest := sha256.Sum256([]byte(summary))
		_ = d.Control.CompleteMaintenanceJob(ctx, job.TenantID, job.RunID, job.AnalyzerVersion, hex.EncodeToString(digest[:8]))
	}
}
