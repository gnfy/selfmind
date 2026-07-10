package httpapi

import (
	"context"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/platform/log"
)

type TaskGovernanceOptions struct {
	InboxEnabled              bool
	DefaultListLimit          int
	AutoArchiveDoneAfter      time.Duration
	AutoArchiveCancelledAfter time.Duration
}

func (o TaskGovernanceOptions) listLimit() int {
	if o.DefaultListLimit <= 0 {
		return 10
	}
	if o.DefaultListLimit > 50 {
		return 50
	}
	return o.DefaultListLimit
}

const taskGovernanceSweepInterval = 6 * time.Hour

// StartTaskGovernanceSweeper runs reversible terminal-task archiving at boot
// and periodically. It never archives open/interrupted/waiting/pinned work and
// never deletes runs, events, handoffs, or artifacts.
func (d *Server) StartTaskGovernanceSweeper(ctx context.Context) func() {
	if d == nil || d.Control == nil {
		return func() {}
	}
	sweepCtx, cancel := context.WithCancel(ctx)
	d.runTaskGovernanceSweep(sweepCtx)
	go func() {
		ticker := time.NewTicker(taskGovernanceSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-ticker.C:
				d.runTaskGovernanceSweep(sweepCtx)
			}
		}
	}()
	return cancel
}

func (d *Server) runTaskGovernanceSweep(ctx context.Context) int {
	archived, err := d.Control.ArchiveStaleTasks(ctx, time.Now(),
		d.TaskGovernance.AutoArchiveDoneAfter,
		d.TaskGovernance.AutoArchiveCancelledAfter)
	if err != nil {
		log.Warn("gateway: task governance sweep failed", "error", err)
		return 0
	}
	for _, task := range archived {
		_, _ = d.Control.AppendEvent(context.WithoutCancel(ctx), control.Event{
			TaskID:     task.TaskID,
			Type:       "task.archived",
			Visibility: "task",
			Payload: mustJSON(map[string]string{
				"reason":          "automatic retention policy",
				"previous_status": task.Status,
			}),
		})
	}
	if len(archived) > 0 {
		log.Info("gateway: archived stale terminal tasks", "count", len(archived))
	}
	return len(archived)
}
