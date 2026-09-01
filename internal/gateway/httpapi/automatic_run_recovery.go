package httpapi

import (
	"context"
	"fmt"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/platform/log"
)

func recoveryModeFromQueueKey(key string) string {
	parts := strings.Split(strings.TrimSpace(key), ":")
	if len(parts) >= 3 {
		switch parts[len(parts)-1] {
		case control.RunRecoveryModeContinue, control.RunRecoveryModeVerifyOnly:
			return parts[len(parts)-1]
		}
	}
	return control.RunRecoveryModeContinue
}

func automaticRecoveryContent(mode string) string {
	if mode == control.RunRecoveryModeVerifyOnly {
		return "Recover the interrupted run in verification-only mode. Observe the current state of every uncertain effect without repeating it. Resolve the uncertainty from read-only evidence, then finish with the verified state or an actionable blocker."
	}
	return "Continue the interrupted run from its durable plan and evidence. Preserve completed steps, choose a genuinely different strategy after a recorded failure, and finish with a structured outcome."
}

// scheduleAutomaticRunRecovery consumes the control plane's typed eligibility
// decision and creates at most one exact-parent queue row. Returning scheduled
// means either this call or an earlier replay materialized that same row.
func (d *Server) scheduleAutomaticRunRecovery(ctx context.Context, item control.RecoveryNotification, drain bool) (scheduled bool, err error) {
	if d == nil || d.Control == nil || strings.TrimSpace(item.RunID) == "" {
		return false, nil
	}
	if d.DisableAutomaticRunRecovery {
		return false, nil
	}
	decision, err := d.Control.AutomaticRunRecoveryDecisionForRun(ctx, item.TenantID, item.RunID)
	if err != nil || !decision.Eligible {
		return false, err
	}
	run, err := d.Control.GetRun(ctx, item.TenantID, item.RunID)
	if err != nil || run == nil {
		return false, err
	}
	task, err := d.Control.GetTask(ctx, item.TenantID, run.TaskID)
	if err != nil || task == nil || task.PersonID != run.PersonID {
		return false, err
	}
	item.PersonID = run.PersonID
	item.TaskID = run.TaskID
	item.Channel = fallback(item.Channel, run.Channel)
	key := fmt.Sprintf("run-recovery:%s:%s", run.ID, decision.Mode)
	route := d.routeIdentityForPerson(ctx, item.TenantID, item.PersonID, item.Channel, "", nil)
	queued, err := d.Control.EnqueueQueued(ctx, control.QueuedTask{
		TenantID: item.TenantID, PersonID: item.PersonID,
		Platform: route.Platform, PlatformUserID: route.PlatformUserID,
		Channel: fallback(item.Channel, route.Platform), Content: automaticRecoveryContent(decision.Mode),
		WorkspaceID: task.WorkspaceID, ExecutionRoots: executionenv.CloneRootBindings(run.ExecutionRoots),
		TaskID: task.ID, ReplyToRunID: run.ID, IdempotencyKey: key,
		Class: control.QueueClassRecovery,
	})
	if err != nil || queued == nil {
		return false, err
	}
	if err := d.Control.MarkAutomaticRecoveryScheduled(ctx, item, decision.Mode, queued.ID); err != nil {
		return false, err
	}
	log.Warn("gateway: scheduled automatic run recovery", "run_id", run.ID, "mode", decision.Mode, "queue_id", queued.ID)
	if drain {
		d.coordinator().drainQueue(route)
	}
	return true, nil
}
