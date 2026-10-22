package httpapi

import (
	"context"
	"fmt"
	"strings"

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
		if err := d.enqueueExternalResourceWake(ctx, wait.ExternalResourceWait, "waiting_external"); err != nil {
			log.Warn("external resource wakeup failed", "run_id", wait.RunID, "error", err)
		}
	}
	// A Run blocked because an effect it needed was left unresolved resumes
	// through the same idempotent continuation once every claim is released.
	released, err := d.Control.ListWakeableBlockedResourceWaits(ctx, 100)
	if err != nil {
		log.Warn("blocked resource wait scan failed", "error", err)
	}
	for _, wait := range released {
		if err := d.enqueueExternalResourceWake(ctx, wait, "blocked"); err != nil {
			log.Warn("released resource wakeup failed", "run_id", wait.RunID, "error", err)
		}
	}
	d.notifyResourceObservationRequired(ctx)
}

// resourceWaitClaimSummary names the claims a parked wait is blocked on,
// bounded for an outcome or a notice.
func resourceWaitClaimSummary(claims []control.ExternalEffectClaim) (string, string) {
	var lines []string
	seen := map[string]bool{}
	first := ""
	for _, claim := range claims {
		key := claim.RunID + "\x00" + claim.EffectID
		if seen[key] {
			continue
		}
		seen[key] = true
		if first == "" {
			first = claim.ID
		}
		if len(lines) < 3 {
			lines = append(lines, claim.Description())
		}
	}
	return strings.Join(lines, "; "), first
}

// resourceWaitResumeStep says what /resume does for a wait that needs a
// decision. An unresolved effect of the Run's own chain is never passed by
// asking: the continuation observes it read-only first. Another Run's
// left-uncertain effect asks the person once, unless the Run observes it
// read-only first.
func resourceWaitResumeStep(runID string, claims []control.ExternalEffectClaim) string {
	own, foreign := false, false
	for _, claim := range claims {
		switch {
		case claim.Disposition == control.ExternalClaimWatched:
		case claim.SameChain:
			own = true
		case claim.Disposition == control.ExternalClaimLeftUncertain:
			foreign = true
		}
	}
	step := "Or /resume " + runID + " to continue now: "
	switch {
	case own && foreign:
		return step + "the run first observes its own earlier effect read-only; another run's effect then asks you once, unless the run observes it read-only too."
	case own:
		return step + "the run first observes its own earlier effect read-only and binds that check to the claim."
	}
	return step + "the conflicting call asks you once, or the run first observes the effect read-only."
}

// ownWatchedResourceWait reports a wait held only by effects of the waiting
// Run's own chain that a bound watcher still observes.
func ownWatchedResourceWait(claims []control.ExternalEffectClaim) bool {
	for _, claim := range claims {
		if !claim.SameChain || claim.Disposition != control.ExternalClaimWatched {
			return false
		}
	}
	return len(claims) > 0
}

func (d *Server) blockUnobservableResourceWait(ctx context.Context, wait control.ExternalResourceWaitProjection) error {
	run, err := d.Control.GetRun(ctx, wait.TenantID, wait.RunID)
	if err != nil {
		return err
	}
	if run == nil {
		return fmt.Errorf("resource wait run missing")
	}
	claims, first := resourceWaitClaimSummary(wait.BlockingClaims)
	outcome, _ := d.coordinator().latestStructuredRunOutcome(ctx, run.TaskID, run.ID)
	outcome.Status, outcome.CompletionReason, outcome.Resumable = "blocked", "external_effect_unresolved", true
	outcome.Summary = "This work waits on an external effect that was left unresolved (" + claims + "). Its conflicting call was not dispatched. It continues by itself once that claim is released."
	outcome.NextSteps = []string{
		"If you know the effect's result, settle it with /effects settle " + first + " [reason]; this work then continues automatically.",
		resourceWaitResumeStep(run.ID, wait.BlockingClaims),
	}
	_, err = d.Control.MaterializeRunFinalization(ctx, control.RunFinalization{
		Identity: control.IdentityContext{TenantID: wait.TenantID, PersonID: wait.PersonID},
		RunID:    run.ID, TaskID: run.TaskID, RunStatus: "blocked", ExpectedRunStatus: "waiting_external", ResourceWait: &wait.ExternalResourceWait,
		Channel: run.Channel, Summary: outcome.Summary, NextSteps: outcome.NextSteps,
		Handoff: control.Handoff{Summary: outcome.Summary, DoneItems: outcome.Done, NextSteps: outcome.NextSteps, ChangedFiles: outcome.Files, Risks: outcome.Risks},
		Event: control.Event{Type: "run.finished", Channel: run.Channel, Visibility: "task", IdempotencyKey: "resource-observation:" + run.ID + ":" + wait.EffectID,
			Payload: mustJSON(map[string]interface{}{"outcome": outcome, "resource_observation_required": true, control.ResourceWakeOnReleasePayloadKey: true})},
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
			outcome = api.RunOutcome{Status: "blocked", Summary: "This work waits on an unresolved external effect. Use /effects to settle it, or /resume " + item.RunID + " to continue now."}
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

// enqueueExternalResourceWake queues the exact continuation of a parked Run
// whose targets are free. parkedStatus is the Run status the caller found:
// waiting_external, or blocked for a correction that promised a wakeup.
func (d *Server) enqueueExternalResourceWake(ctx context.Context, wait control.ExternalResourceWait, parkedStatus string) error {
	run, err := d.Control.GetRun(ctx, wait.TenantID, wait.RunID)
	if err != nil {
		return err
	}
	if run == nil || run.PersonID != wait.PersonID || run.Status != parkedStatus {
		return fmt.Errorf("resource wait parent changed before wakeup")
	}
	if err := d.Control.MarkExternalResourceWaitQueued(ctx, wait); err != nil {
		return err
	}
	origin := d.routeIdentityForPerson(ctx, wait.TenantID, wait.PersonID, run.Channel, "cli", nil)
	if origin == nil || origin.PersonID != wait.PersonID {
		return fmt.Errorf("resource wait owner cannot be routed")
	}
	row := control.QueuedTask{
		TenantID: wait.TenantID, PersonID: wait.PersonID,
		Channel: run.Channel, Platform: origin.Platform, PlatformUserID: origin.PlatformUserID,
		TaskID: run.TaskID, ReplyToRunID: run.ID, Class: control.QueueClassFinalization,
		WorkspaceID: run.WorkspaceID, ExecutionRoots: run.ExecutionRoots,
		Content:        "Continue this exact work after the external target became available. The earlier conflicting tool call was not dispatched. Check the current target state before any new mutation.",
		IdempotencyKey: "external-resource:" + wait.RunID + ":" + wait.EffectID,
	}
	row.Admission = d.enqueueContinuationAdmission(ctx, row)
	_, err = d.Control.EnqueueQueued(ctx, row)
	if err != nil {
		return err
	}
	d.coordinator().drainQueue(origin)
	return nil
}
