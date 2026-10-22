package httpapi

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"selfmind/internal/control"
)

const externalEffectUsage = "Usage: /effects [settle <claim_id> [reason] | resolve <claim_id> <watch_id>]"

// effectsCommandReply is the person's model-free view of unresolved external
// effects and the two release paths only the person can take: settling an
// effect whose result they know, and relating a finalized watcher to it.
func (d *Server) effectsCommandReply(ctx context.Context, identity *control.IdentityContext, channel string, args []string) (string, error) {
	if d == nil || d.Control == nil || identity == nil {
		return "External effects unavailable.", nil
	}
	if len(args) > 0 && strings.EqualFold(args[0], "settle") {
		if len(args) < 2 {
			return externalEffectUsage, nil
		}
		return d.settleExternalEffectReply(ctx, identity, channel, args[1], strings.Join(args[2:], " "))
	}
	if len(args) == 3 && strings.EqualFold(args[0], "resolve") {
		watch, userErr, err := d.resolveWatcherReference(ctx, identity, args[2])
		if err != nil || userErr != "" {
			return userErr, err
		}
		claim, err := d.Control.ObserveExternalEffectWithWatch(ctx, identity.TenantID, identity.PersonID, args[1], watch.ID)
		if err != nil {
			return "The effect remains occupied: " + err.Error(), nil
		}
		// The durable wait remains authoritative. An immediate pass improves
		// latency; the daemon's next pass retries if queueing fails here.
		d.runExternalResourceWaitPass(ctx)
		return fmt.Sprintf("Released external effect %s using finalized watcher %s. Waiting work will resume when admitted.", claim.ID, shortExternalWatchID(watch.ID)), nil
	}
	if len(args) != 0 {
		return externalEffectUsage, nil
	}
	claims, err := d.Control.ListUnresolvedExternalEffects(ctx, identity.TenantID, identity.PersonID, 20)
	if err != nil {
		return "", err
	}
	if len(claims) == 0 {
		return "No unresolved external effects.", nil
	}
	var sb strings.Builder
	sb.WriteString("Unresolved external effects:\n")
	unknownTarget := false
	for _, claim := range claims {
		state := "uncertain"
		switch {
		case claim.Disposition == control.ExternalClaimInFlight:
			state = "in flight"
		case claim.Disposition == control.ExternalClaimWatched:
			state = "watched"
		case claim.State == control.ExternalClaimReserved:
			state = "never dispatched; released automatically"
		}
		summary := "call summary unavailable"
		if claim.Summary != "" {
			summary = "`" + claim.Summary + "`"
		}
		if claim.Outcome != "" {
			summary += ", " + claim.Outcome
		} else if state == "uncertain" {
			summary += ", effect outcome not established"
			if !claim.DispatchRecorded {
				summary += "; dispatch details unavailable"
			}
		}
		target := claim.TargetKey
		if target == control.UnknownExternalTarget {
			unknownTarget = true
			target += " (unknown; conflicts with all your external targets)"
		}
		fmt.Fprintf(&sb, "- %s | run %s | target %s | %s | %s", claim.ID, claim.RunID, target, state, summary)
		if !claim.CreatedAt.IsZero() {
			fmt.Fprintf(&sb, " | since %s", claim.CreatedAt.UTC().Format("2006-01-02 15:04:05 UTC"))
		}
		sb.WriteByte('\n')
	}
	if unknownTarget {
		sb.WriteString("Unknown target scope prevents proving that another external action is independent. A one-time approval leaves the original claim unresolved, so later conflicting actions may ask again.\n")
	}
	sb.WriteString("If you know an uncertain effect's result, settle it with /effects settle <claim_id> [reason]; waiting work then continues. " +
		"Its run can instead release it with a successful read-only observation bound to the claim, and /effects resolve <claim_id> <watch_id> relates a finalized watcher to it. " +
		"An effect in flight settles by itself when its call succeeds. Age and approval of a different action never release an uncertain effect; missing lifecycle details are not proof that it never ran.")
	return sb.String(), nil
}

// runningBackgroundEffectReply explains a settlement refused because the
// effect is a background process that is still running: it names the process
// and says how it stops, since only its exit ends the hold. It returns "" for
// any other in-flight effect.
func (d *Server) runningBackgroundEffectReply(ctx context.Context, identity *control.IdentityContext, claimID string) string {
	claim, err := d.Control.ExternalEffectClaimDetail(ctx, identity.TenantID, identity.PersonID, claimID)
	if err != nil || !claim.Background || claim.Disposition != control.ExternalClaimInFlight {
		return ""
	}
	process := "a background process"
	if claim.Summary != "" {
		process = "the background process `" + claim.Summary + "`"
	}
	stop := "Its run has ended, so the process stops at its time limit (2 hours by default) or when SelfMind restarts."
	if run, err := d.Control.GetRun(ctx, identity.TenantID, claim.RunID); err == nil && run != nil && run.Status == "running" {
		stop = "While run " + claim.RunID + " is running it can stop the process with the process tool (action kill); " +
			"otherwise the process stops at its time limit (2 hours by default) or when SelfMind restarts."
	}
	return fmt.Sprintf("External effect %s is %s, started by run %s; the process is still running, so its result is not known yet. %s "+
		"Its exit then settles the effect if it exited 0, or leaves it for you to settle with /effects settle %s [reason].",
		claimID, process, claim.RunID, stop, claimID)
}

// settleExternalEffectReply records the person's settlement through the
// audited store path: the actor account and channel are this request's own.
func (d *Server) settleExternalEffectReply(ctx context.Context, identity *control.IdentityContext, channel, claimID, reason string) (string, error) {
	result, err := d.Control.SettleExternalEffect(ctx, control.ExternalEffectSettlementRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, ClaimID: claimID,
		ActorAccountID: identity.AccountID, ActorChannel: channel, Reason: reason,
	})
	switch {
	case errors.Is(err, control.ErrExternalEffectClaimNotFound):
		return fmt.Sprintf("No unresolved external effect %s belongs to you.", claimID), nil
	case errors.Is(err, control.ErrExternalEffectInFlight):
		if text := d.runningBackgroundEffectReply(ctx, identity, claimID); text != "" {
			return text, nil
		}
		return fmt.Sprintf("External effect %s is still in flight: its call has not returned, so its result is not known yet. It settles by itself if the call succeeds; settle it after the call ends if it stays unresolved.", claimID), nil
	case errors.Is(err, control.ErrExternalEffectNotSettleable):
		return fmt.Sprintf("External effect %s has nothing to settle: it is already released, or its call never started and is released automatically.", claimID), nil
	case errors.Is(err, control.ErrExternalEffectSettlementActor):
		return "This endpoint cannot settle effects for you: settle it from one of your own active accounts.", nil
	case err != nil:
		return "", err
	}
	if result.Replayed {
		return fmt.Sprintf("External effect %s was already settled (%s).", claimID, result.Settlement.ID), nil
	}
	d.runExternalResourceWaitPass(ctx)
	text := fmt.Sprintf("Settled external effect %s of run %s on your word", claimID, result.Settlement.RunID)
	if result.Settlement.Reason != "" {
		text += ": " + result.Settlement.Reason
	}
	return text + ". Work waiting on it continues when admitted.", nil
}
