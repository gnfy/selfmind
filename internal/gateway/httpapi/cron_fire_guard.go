package httpapi

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/kernel/task/cron"
	"selfmind/internal/platform/log"
	"selfmind/internal/platform/textutil"
)

// CronJobLookup is how the drain asks whether the schedule a queued fire came
// from still exists. cron.Scheduler implements it.
type CronJobLookup interface {
	GetJob(ctx context.Context, tenantID string, id int64) (*cron.CronJob, error)
}

// cronJobs is the lookup in force: the one a test or deployment installed,
// else the scheduler, else none.
func (d *Server) cronJobs() CronJobLookup {
	if d == nil {
		return nil
	}
	if d.CronJobs != nil {
		return d.CronJobs
	}
	if d.Schedules != nil {
		return d.Schedules
	}
	return nil
}

// queuedCronFireVerdict is what the drain decides about a scheduled fire that
// waited in the queue.
type queuedCronFireVerdict int

const (
	// cronFireRuns: start it, as an ordinary queued row.
	cronFireRuns queuedCronFireVerdict = iota
	// cronFireDropped: the schedule it came from is gone or switched off, so
	// the fire is cancelled and the person told. Nothing ran for it.
	cronFireDropped
	// cronFireUnknown: the lookup failed. The row is not claimed and stays
	// queued for a later drain; it is neither started nor dropped on a guess.
	cronFireUnknown
)

// queuedCronFire decides whether a queued row that a schedule's fire wrote may
// still start. A fire was authorized by the schedule the person created, and
// the queue can hold it long enough for that schedule to be deleted or
// switched off, so the authority it carries is no longer there to start under.
//
// Only a fresh scheduled fire is asked about. Continuations inherit the cron
// authorization origin, but their queue trigger names the work already started.
// Deleting a schedule does not cancel that work or discard a recorded answer. A job that
// is switched off is dropped only when it recurs: a one-time job is disabled
// right after its first attempt, so its fire waiting in the queue is exactly
// the work it was created for. Rows with no snapshot, rows that are not
// scheduled, and a deployment with no scheduler are not asked about.
func (d *Server) queuedCronFire(ctx context.Context, row control.QueuedTask) (queuedCronFireVerdict, string, error) {
	lookup := d.cronJobs()
	if lookup == nil || row.Admission.Origin.Kind != runOriginCron || queuedTrigger(row) != runOriginCron ||
		row.RunID != "" || row.ReplyToRunID != "" || row.ClarifyID != "" {
		return cronFireRuns, "", nil
	}
	id, err := strconv.ParseInt(strings.TrimSpace(row.Admission.Origin.ID), 10, 64)
	if err != nil || id <= 0 {
		return cronFireRuns, "", nil
	}
	job, err := lookup.GetJob(ctx, row.TenantID, id)
	switch {
	case errors.Is(err, cron.ErrJobNotFound):
		return cronFireDropped, "its schedule was deleted", nil
	case err != nil:
		return cronFireUnknown, "", err
	case !job.Enabled && !job.Once:
		return cronFireDropped, "its schedule was switched off", nil
	}
	return cronFireRuns, "", nil
}

// dropOrphanedCronFire cancels a queued scheduled fire whose schedule is gone
// or off, after telling the person once. The notice goes first, through the
// same two durable surfaces the unreadable-admission notice uses (an event on
// the work's thread when the row has one, and a push recorded in the outbox),
// both idempotent on the row, so a drain that cannot cancel yet tells nobody
// twice. It reports whether the row was cancelled; when it was not, the row
// stays queued and the next drain tries again.
func (c *RunCoordinator) dropOrphanedCronFire(ctx context.Context, identity *control.IdentityContext, row control.QueuedTask, why string) bool {
	if c == nil || c.srv == nil || c.srv.Control == nil {
		return false
	}
	content := fmt.Sprintf("A scheduled run (%q) did not start: %s while it waited, so nothing ran. "+
		"Create the schedule again if you still want it.", textutil.Truncate(toOneLine(row.Content), 60), why)
	key := "cron-fire-dropped:" + row.ID
	recorded := strings.TrimSpace(row.TaskID) == ""
	if !recorded {
		_, err := c.srv.Control.AppendEvent(ctx, control.Event{TaskID: row.TaskID, TenantID: row.TenantID, PersonID: row.PersonID,
			Type: "queue.cron_fire_dropped", Visibility: "task", Channel: row.Channel, IdempotencyKey: key,
			Payload: mustJSON(map[string]string{"queue_id": row.ID, "job_id": row.Admission.Origin.ID, "message": content})})
		recorded = err == nil
		if err != nil {
			log.Warn("gateway: the dropped scheduled fire could not be recorded on its thread; it stays queued", "queue_id", row.ID, "error", err)
		}
	}
	pushed := c.queueNoticePush(ctx, identity, row, delivery.Message{TenantID: row.TenantID, PersonID: row.PersonID,
		TaskID: row.TaskID, Kind: delivery.KindRecovery, Content: content, LogicalKey: key}, recorded)
	if !recorded || !pushed {
		return false
	}
	c.srv.events().publish(api.RunEvent{TenantID: row.TenantID, PersonID: row.PersonID, Type: "background.notice",
		Durability: api.EventEphemeral, CreatedAt: time.Now(),
		Payload: mustJSON(map[string]string{"message": content, "kind": "warning"})})
	if err := c.srv.Control.MarkQueued(ctx, row.TenantID, row.ID, control.QueueStatusCancelled); err != nil {
		log.Warn("gateway: the dropped scheduled fire could not be cancelled; it stays queued", "queue_id", row.ID, "error", err)
		return false
	}
	return true
}
