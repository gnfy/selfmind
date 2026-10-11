package httpapi

import (
	"context"
	"strings"

	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/task/cron"
	"selfmind/internal/platform/log"
)

// CanaryPrefix marks a cron job as a liveness self-check (W0.3). The executor
// runs the rest of the prompt as a normal turn but delivers ONLY on failure, so
// a healthy system stays silent and a broken one pings you.
const CanaryPrefix = "canary:"

// CronExecutor adapts the gateway Server to cron.JobExecutor: it runs a cron
// job's prompt as a real agent turn — as its creating person through their
// own bound endpoint, in their resolved workspace — and delivers the result to
// the job's channel. This is what makes scheduled jobs proactive (e.g. a daily
// summary pushed to WeChat) rather than leaving a marker for the next chat.
//
// It is wired by the gateway runner after the Server and its Delivery service
// are ready, then handed to the scheduler via Scheduler.SetExecutor before
// Start(). In CLI-only mode (no Server) the scheduler keeps its marker fallback.
type CronExecutor struct {
	srv *Server
	// sched, when set, lets the executor persist the job's learned task
	// binding (W6). Nil keeps the pre-binding behavior (pre-label each fire).
	sched *cron.Scheduler
}

// NewCronExecutor returns a JobExecutor backed by the gateway Server. The
// scheduler reference enables stable task binding for recurring jobs.
func NewCronExecutor(srv *Server, sched *cron.Scheduler) *CronExecutor {
	return &CronExecutor{srv: srv, sched: sched}
}

// RunCronJob runs the job's prompt synchronously and delivers the result. It
// runs inside robfig/cron's per-job goroutine, so a long turn does not block
// other jobs. A "busy" outcome (the person was mid-run) is left undelivered by
// deliverCronResult and simply retried on the next schedule.
func (e *CronExecutor) RunCronJob(ctx context.Context, job cron.CronJob) error {
	_, err := e.Fire(ctx, job)
	return err
}

// Fire runs one job once, delivers its result, and returns the gateway's
// answer. The job runs only as an account already bound to a person and
// delivers only to that account's own chat; otherwise nothing runs, nothing
// reaches the job's endpoint, and the job is turned off so it is not refused
// again at every fire.
func (e *CronExecutor) Fire(ctx context.Context, job cron.CronJob) (api.MessageResponse, error) {
	if e == nil || e.srv == nil {
		return api.MessageResponse{}, nil
	}
	prompt := job.Prompt
	canary := strings.HasPrefix(prompt, CanaryPrefix)
	if canary {
		prompt = strings.TrimSpace(strings.TrimPrefix(prompt, CanaryPrefix))
		if prompt == "" {
			prompt = "Reply with the single word READY to confirm you are working. Do not use any tools."
		}
	}
	fire, err := e.srv.resolveScheduledFire(ctx, job)
	if err != nil {
		return api.MessageResponse{Error: err.Error()}, err
	}
	if fire.refusal != "" {
		turnedOff := false
		if e.sched != nil {
			if err := e.sched.EnableJob(ctx, job.ID, false); err != nil {
				log.Warn("cron: refused job could not be turned off", "job_id", job.ID, "error", err)
			} else {
				turnedOff = true
			}
		}
		notice := refusedScheduleNotice(job, fire, turnedOff)
		e.srv.noticeRefusedSchedule(ctx, job, fire, notice, turnedOff)
		if fire.owner == "" {
			e.srv.noticeUnclaimedSchedules(ctx, e.sched, job.TenantID)
		}
		return api.MessageResponse{Identity: fire.tell, Content: notice,
			Turn: messageTurn("failed", "", "idle", "", "", notice)}, nil
	}
	req := api.MessageRequest{
		TenantID:       fire.account.TenantID,
		Platform:       fire.account.Platform,
		PlatformUserID: fire.account.PlatformUserID,
		Channel:        fire.channel,
		Content:        prompt,
		AllowWeb:       job.Web,
		// A schedule fired this turn, not the person. Attached clients render
		// it as a result line instead of replaying its progress (run.started
		// carries the origin; the ctx tag below does not survive an async run).
		Origin:   runOriginCron,
		OriginID: cronOriginID(job.ID),
		// The person authorised this work when they created the schedule, so
		// the turn may use the remembered classes that existed at that instant
		// and no later ones. A job with no authorisation instant uses none and
		// asks, which is what an unattended fire did before standing classes
		// existed.
		StandingGrantsAuthorizedAt: job.AuthorizedAt,
	}
	// Stable label binding (W6): a learned task id rides the request as
	// explicit attach evidence, so every fire of a daily job lands on the
	// same label instead of a fresh pre-label guess. A binding whose label
	// has been archived, or belongs to someone other than the person the job
	// runs as, is cleared and re-learned from this run.
	if strings.TrimSpace(job.TaskID) != "" && e.srv.Control != nil {
		task, err := e.srv.Control.GetTask(ctx, job.TenantID, job.TaskID)
		if err == nil && task != nil && task.PersonID == fire.account.PersonID && task.ArchivedAt == nil && !archivedTaskStatus(task.Status) {
			req.TaskID = job.TaskID
		} else {
			if e.sched != nil {
				_ = e.sched.SetTaskID(ctx, job.ID, "")
			}
			job.TaskID = ""
		}
	}
	// Tag the turn's origin so its work-spine entry reads "[cron] …" — a
	// non-interactive turn must be distinguishable from the person's own
	// message when the spine tail is replayed later. The sync run ctx keeps
	// values (WithoutCancel); a fire queued behind busy work gets the tag back
	// from the cron queue class it was written with, and its authority from the
	// admission snapshot it keeps.
	ctx = kernel.WithTurnSource(ctx, "cron")
	ctx = withExpectedScheduleCreator(ctx, fire.account.TenantID, fire.owner)
	resp, _ := e.srv.ProcessMessage(ctx, req)
	// Admission resolves the account again. A removed or moved endpoint must
	// neither dispatch as its new owner nor receive the refused fire's output.
	if resp.Identity == nil || resp.Identity.PersonID != fire.owner || resp.Identity.TenantID != fire.account.TenantID {
		return resp, nil
	}
	// The admitted work stays the creator's work when a binding changes
	// during execution. Its result may leave only through a still-owned chat.
	current, deliveryErr := e.srv.resolveScheduledFire(ctx, job)
	canDeliver := deliveryErr == nil && current.refusal == "" && current.account != nil && current.owner == fire.owner
	if canDeliver {
		req.Channel = current.channel
	} else {
		log.Warn("cron: result not delivered through a refused endpoint", "job_id", job.ID, "reason", current.refusal, "error", deliveryErr)
	}
	if canary {
		// Liveness check: stay silent on success, alert only on failure.
		if canDeliver && cronRunFailed(resp) {
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
		return resp, nil
	}
	if canDeliver {
		e.srv.coordinator().deliverCronResult(ctx, req, resp)
	}
	// Learn the binding from the first successful execution: the resolved
	// task is this job's stable label from now on.
	if job.TaskID == "" && e.sched != nil && resp.Error == "" && resp.Task != nil && strings.TrimSpace(resp.Task.ID) != "" {
		_ = e.sched.SetTaskID(ctx, job.ID, resp.Task.ID)
	}
	return resp, nil
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
