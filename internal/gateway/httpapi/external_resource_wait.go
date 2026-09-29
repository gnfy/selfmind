package httpapi

import (
	"context"
	"fmt"

	"selfmind/internal/control"
	"selfmind/internal/platform/log"
)

// runExternalResourceWaitPass runs with the daemon's existing durable-watch
// worker. It starts no process and consumes no model worker while a target is
// occupied. A queued wakeup does not replay the failed tool call: its exact
// child Run must inspect current state and submit a new call through the claim
// gate, which rechecks conflicts before any effect.
func (d *Server) runExternalResourceWaitPass(ctx context.Context) {
	if d == nil || d.Control == nil {
		return
	}
	if _, err := d.Control.ObserveUndispatchedExternalEffects(ctx); err != nil {
		log.Warn("undispatched external effect recovery failed", "error", err)
		return
	}
	waits, err := d.Control.ListReadyExternalResourceWaits(ctx, 100)
	if err != nil {
		log.Warn("external resource wait scan failed", "error", err)
		return
	}
	for _, wait := range waits {
		if err := d.enqueueExternalResourceWake(ctx, wait); err != nil {
			log.Warn("external resource wakeup failed", "run_id", wait.RunID, "error", err)
		}
	}
}

func (d *Server) enqueueExternalResourceWake(ctx context.Context, wait control.ExternalResourceWait) error {
	run, err := d.Control.GetRun(ctx, wait.TenantID, wait.RunID)
	if err != nil {
		return err
	}
	if run == nil || run.PersonID != wait.PersonID || run.Status != "waiting_external" {
		return fmt.Errorf("resource wait parent changed before wakeup")
	}
	if err := d.Control.MarkExternalResourceWaitQueued(ctx, wait); err != nil {
		return err
	}
	origin := d.routeIdentityForPerson(ctx, wait.TenantID, wait.PersonID, run.Channel, "cli", nil)
	if origin == nil || origin.PersonID != wait.PersonID {
		return fmt.Errorf("resource wait owner cannot be routed")
	}
	_, err = d.Control.EnqueueQueued(ctx, control.QueuedTask{
		TenantID: wait.TenantID, PersonID: wait.PersonID,
		Channel: run.Channel, Platform: origin.Platform, PlatformUserID: origin.PlatformUserID,
		TaskID: run.TaskID, ReplyToRunID: run.ID, Class: control.QueueClassFinalization,
		WorkspaceID: run.WorkspaceID, ExecutionRoots: run.ExecutionRoots,
		Content:        "Continue this exact work after the external target became available. The earlier conflicting tool call was not dispatched. Check the current target state before any new mutation.",
		IdempotencyKey: "external-resource:" + wait.RunID + ":" + wait.EffectID,
	})
	if err != nil {
		return err
	}
	d.coordinator().drainQueue(origin)
	return nil
}
