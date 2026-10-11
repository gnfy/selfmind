package httpapi

import (
	"fmt"
	"time"

	"selfmind/internal/control"
)

func formatResourceWaitBacklog(waits []control.ExternalResourceWaitProjection, now time.Time) string {
	var pending, ready, observation int
	var oldest time.Time
	for _, wait := range waits {
		if oldest.IsZero() || wait.CreatedAt.Before(oldest) {
			oldest = wait.CreatedAt
		}
		if wait.Ready {
			ready++
		} else if wait.RunStatus == "blocked" || wait.NeedsObservation {
			observation++
		} else {
			pending++
		}
	}
	age := "none"
	if !oldest.IsZero() {
		age = now.Sub(oldest).Round(time.Second).String()
	}
	return fmt.Sprintf("External resource waits now: waiting on an in-flight effect %d, ready %d, waiting on an unresolved effect %d, oldest %s (bounded to 100 Runs).\n", pending, ready, observation, age)
}

func formatResourceWaitDetail(wait control.ExternalResourceWaitProjection) string {
	if wait.RunStatus == "blocked" && wait.Ready {
		if wait.WakeOnRelease {
			return "The held effect has been released; this work continues automatically when admitted."
		}
		return "The held effect has been released. Use /resume " + wait.RunID + " to assess and finish the remaining work."
	}
	if wait.RunStatus == "blocked" || wait.NeedsObservation {
		claims, first := resourceWaitClaimSummary(wait.BlockingClaims)
		return "This work waits on an unresolved external effect (" + claims + "); its conflicting call was not dispatched. " +
			"If you know the effect's result, settle it with /effects settle " + first + " [reason]. " +
			resourceWaitResumeStep(wait.RunID, wait.BlockingClaims)
	}
	if wait.Ready {
		return "External target is available; the exact continuation is waiting for dispatch."
	}
	// The wait is for the claims in flight or watched; another Run's
	// left-uncertain claim is decided after it, as the claim gate orders them.
	var waited, decide []control.ExternalEffectClaim
	for _, claim := range wait.BlockingClaims {
		if claim.Disposition == control.ExternalClaimLeftUncertain && !claim.SameChain {
			decide = append(decide, claim)
		} else {
			waited = append(waited, claim)
		}
	}
	text := "External target is held by another run's in-flight call or by a watcher bound to it; "
	then := "this work continues when that claim is released."
	if ownWatchedResourceWait(waited) {
		claims, first := resourceWaitClaimSummary(waited)
		text = "External target is held by an earlier effect of this work that a watcher bound to it still observes (" + claims + "); "
		then = "this work continues when that watcher releases it, or settle it with /effects settle " + first + " [reason]."
	}
	if len(decide) > 0 {
		claims, first := resourceWaitClaimSummary(decide)
		then = "once it is released, this work still needs your decision on another run's unresolved effect (" + claims +
			"): settle it with /effects settle " + first + " [reason] and the work continues by itself, or then /resume " +
			wait.RunID + " and approve the conflicting call once."
	}
	return text + then + " The blocked call was not dispatched."
}
