package httpapi

import (
	"context"

	"selfmind/internal/control"
	"selfmind/internal/kernel/task/cron"
)

// ScheduleReview is the stored schedules as one person reviewing them sees
// them: their own and the unclaimed ones in full, everyone else's only as a
// count.
type ScheduleReview struct {
	Own       int
	Unclaimed int
	Others    int
	// Stuck lists the reviewer's own and the unclaimed jobs that do not run.
	Stuck []StuckSchedule
}

// StuckSchedule is one job that does not run, and why.
type StuckSchedule struct {
	ID        int64
	Name      string
	Enabled   bool
	Unclaimed bool
	// NamesNoAccount marks an older row the local command line may adopt.
	NamesNoAccount bool
	Reason         string
}

// ReviewSchedules reads stored jobs, without changing anything, for one
// reviewing person. A job no one can be shown to own is shown, as in the cron
// tool, only to a person with a command-line account, who may remove it.
func ReviewSchedules(ctx context.Context, store *control.Store, reviewer *control.IdentityContext, jobs []cron.CronJob) (ScheduleReview, error) {
	var review ScheduleReview
	commandLine, err := commandLineAccount(ctx, store, reviewer.PersonID)
	if err != nil {
		return ScheduleReview{}, err
	}
	for _, job := range jobs {
		// Read-only: the daemon records a decision when it first meets the job.
		view, err := viewSchedule(ctx, store, job, false)
		if err != nil {
			return ScheduleReview{}, err
		}
		switch {
		case view.owner == "" && commandLine != nil:
			review.Unclaimed++
		case view.owner != "" && view.owner == reviewer.PersonID:
			review.Own++
		default:
			review.Others++
			continue
		}
		if view.refusal != "" {
			review.Stuck = append(review.Stuck, StuckSchedule{ID: job.ID, Name: job.Name, Enabled: job.Enabled,
				Unclaimed: view.owner == "", NamesNoAccount: namesNoAccount(job) && job.SystemKey == "", Reason: view.refusal})
		}
	}
	return review, nil
}
