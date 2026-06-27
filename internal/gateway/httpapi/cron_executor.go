package httpapi

import (
	"context"
	"strings"

	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel/task/cron"
)

// CanaryPrefix marks a cron job as a liveness self-check (W0.3). The executor
// runs the rest of the prompt as a normal turn but delivers ONLY on failure, so
// a healthy system stays silent and a broken one pings you.
const CanaryPrefix = "canary:"

// CronExecutor adapts the gateway Server to cron.JobExecutor: it runs a cron
// job's prompt as a real agent turn — as the person bound to the job's
// platform/recipient, in their resolved workspace — and delivers the result to
// the job's channel. This is what makes scheduled jobs proactive (e.g. a daily
// summary pushed to WeChat) rather than leaving a marker for the next chat.
//
// It is wired by the gateway runner after the Server and its Delivery service
// are ready, then handed to the scheduler via Scheduler.SetExecutor before
// Start(). In CLI-only mode (no Server) the scheduler keeps its marker fallback.
type CronExecutor struct {
	srv *Server
}

// NewCronExecutor returns a JobExecutor backed by the gateway Server.
func NewCronExecutor(srv *Server) *CronExecutor { return &CronExecutor{srv: srv} }

// RunCronJob runs the job's prompt synchronously and delivers the result. It
// runs inside robfig/cron's per-job goroutine, so a long turn does not block
// other jobs. A "busy" outcome (the person was mid-run) is left undelivered by
// deliverCronResult and simply retried on the next schedule.
func (e *CronExecutor) RunCronJob(ctx context.Context, job cron.CronJob) error {
	if e == nil || e.srv == nil {
		return nil
	}
	prompt := job.Prompt
	canary := strings.HasPrefix(prompt, CanaryPrefix)
	if canary {
		prompt = strings.TrimSpace(strings.TrimPrefix(prompt, CanaryPrefix))
		if prompt == "" {
			prompt = "Reply with the single word READY to confirm you are working. Do not use any tools."
		}
	}
	req := api.MessageRequest{
		TenantID:       job.TenantID,
		Platform:       firstNonEmptyString(job.Platform, job.Channel, "cli"),
		PlatformUserID: job.DeliverTo,
		Channel:        firstNonEmptyString(job.Channel, job.Platform, "cli"),
		Content:        prompt,
		AllowWeb:       job.Web,
	}
	resp, _ := e.srv.ProcessMessage(ctx, req)
	if canary {
		// Liveness check: stay silent on success, alert only on failure.
		if cronRunFailed(resp) {
			if resp.Error == "" {
				// Synthesize an error so deliveryContent renders an alert line
				// instead of the "finished" fallback for empty/failed turns.
				reason := "empty response"
				if resp.Turn != nil && resp.Turn.Status == "failed" {
					reason = "turn failed"
				}
				resp.Error = "canary " + reason
			}
			e.srv.coordinator().deliverCronResult(ctx, req, resp)
		}
		return nil
	}
	e.srv.coordinator().deliverCronResult(ctx, req, resp)
	return nil
}

// cronRunFailed reports whether a cron turn did not produce a healthy result:
// an explicit error, a failed turn status, or empty output.
func cronRunFailed(resp api.MessageResponse) bool {
	if resp.Error != "" {
		return true
	}
	if resp.Turn != nil && resp.Turn.Status == "failed" {
		return true
	}
	return strings.TrimSpace(resp.Content) == ""
}
