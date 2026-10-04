package httpapi

import (
	"context"
	"fmt"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/delivery"
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
	waits, err := d.Control.ListActiveExternalResourceWaits(ctx, 100)
	if err != nil {
		log.Warn("external resource wait scan failed", "error", err)
		return
	}
	for _, wait := range waits {
		if wait.RunStatus == "waiting_external" && wait.Status == "pending" && wait.NeedsObservation {
			if err := d.blockUnobservableResourceWait(ctx, wait); err != nil {
				log.Warn("resource observation correction failed", "run_id", wait.RunID, "error", err)
			}
			continue
		}
		if !wait.Ready || wait.RunStatus != "waiting_external" {
			continue
		}
		if err := d.enqueueExternalResourceWake(ctx, wait.ExternalResourceWait); err != nil {
			log.Warn("external resource wakeup failed", "run_id", wait.RunID, "error", err)
		}
	}
	d.notifyResourceObservationRequired(ctx)
}

func (d *Server) blockUnobservableResourceWait(ctx context.Context, wait control.ExternalResourceWaitProjection) error {
	run, err := d.Control.GetRun(ctx, wait.TenantID, wait.RunID)
	if err != nil {
		return err
	}
	if run == nil {
		return fmt.Errorf("resource wait run missing")
	}
	outcome, _ := d.coordinator().latestStructuredRunOutcome(ctx, run.TaskID, run.ID)
	outcome.Status, outcome.CompletionReason, outcome.Resumable = "blocked", "external_effect_unresolved", true
	outcome.Summary = "This work needs an observation of an unresolved external effect. No live run or bound watcher can release the occupied target automatically. The blocked call was not dispatched."
	outcome.NextSteps = []string{"Use /effects to inspect the held claim, then /resume " + run.ID + " to continue with read-only observation. Do not repeat the uncertain effect."}
	_, err = d.Control.MaterializeRunFinalization(ctx, control.RunFinalization{
		Identity: control.IdentityContext{TenantID: wait.TenantID, PersonID: wait.PersonID},
		RunID:    run.ID, TaskID: run.TaskID, RunStatus: "blocked", ExpectedRunStatus: "waiting_external", ResourceWait: &wait.ExternalResourceWait,
		Channel: run.Channel, Summary: outcome.Summary, NextSteps: outcome.NextSteps,
		Handoff: control.Handoff{Summary: outcome.Summary, DoneItems: outcome.Done, NextSteps: outcome.NextSteps, ChangedFiles: outcome.Files, Risks: outcome.Risks},
		Event: control.Event{Type: "run.finished", Channel: run.Channel, Visibility: "task", IdempotencyKey: "resource-observation:" + run.ID + ":" + wait.EffectID,
			Payload: mustJSON(map[string]interface{}{"outcome": outcome, "resource_observation_required": true})},
	})
	return err
}

func (d *Server) notifyResourceObservationRequired(ctx context.Context) {
	items, err := d.Control.ListResourceObservationNotices(ctx)
	if err != nil {
		log.Warn("resource observation notices unavailable", "error", err)
		return
	}
	for _, item := range items {
		origin := d.routeIdentityForPerson(ctx, item.TenantID, item.PersonID, item.Channel, "cli", nil)
		outcome, ok := d.coordinator().latestStructuredRunOutcome(ctx, item.TaskID, item.RunID)
		if !ok {
			outcome = api.RunOutcome{Status: "blocked", Summary: "An external effect needs observation. Use /effects, then /resume " + item.RunID + " for read-only inspection."}
		}
		if !d.coordinator().routePendingNotification(ctx, origin, item.Channel, delivery.Message{
			TenantID: item.TenantID, PersonID: item.PersonID, TaskID: item.TaskID, RunID: item.RunID,
			Kind: delivery.KindRecovery, Content: structuredResultFallback(outcome), LogicalKey: "resource-observation:" + item.EventID,
		}, false) {
			continue
		}
		if _, err := d.Control.AppendEvent(ctx, control.Event{TaskID: item.TaskID, RunID: item.RunID, Type: "run.resource_observation_notified", Visibility: "task", Channel: item.Channel,
			Payload: mustJSON(map[string]string{"source_event_id": item.EventID}), IdempotencyKey: "resource-observation-notified:" + item.EventID}); err != nil {
			log.Warn("resource observation notice marker failed", "error", err)
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
