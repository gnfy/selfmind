package httpapi

import (
	"context"
	"time"

	"selfmind/internal/gateway/api"
	"selfmind/internal/tools"
)

// standingGrantPolicy decides which remembered approval classes one run may
// consume.
//
// Authorization comes from the moment a person decides, not the moment work
// executes, so "is anyone watching right now" is the wrong question. A person's
// own turn carries their decisions. So does a turn the daemon starts to
// continue that same work: an answered approval, a finished watcher, a run
// recovered after a restart — in each the person is the one who asked for the
// work and the run is the one they started.
//
// A schedule is different only in WHEN it was authorised. The person authorised
// it when they created it, so it carries the classes that existed at that
// instant and no later ones: a job created in January must not silently gain
// the classes its owner accepts in June for unrelated interactive work. A job
// with no authorisation instant consumes none and asks, which is exactly what
// an unattended fire did before standing classes existed.
//
// A run may USE a standing class; only a person answering an ask creates one.
// Automatic triage is already confined to a single run by the run-scope grant
// it records on an auto-approval.
//
// Who authorized the work is read from its admission, which every continuation
// restores from its exact parent: an approval answered on a scheduled job's
// work keeps the schedule's instant, whatever trigger started that hop.
func standingGrantPolicy(ctx context.Context, req api.MessageRequest) tools.StandingGrantPolicy {
	if !req.Admission.IsZero() {
		if req.Admission.Origin.Kind != runOriginCron {
			return tools.InteractiveStandingGrants()
		}
		if req.Admission.StandingGrantsAuthorizedAt <= 0 {
			return tools.ScheduledStandingGrants(time.Time{})
		}
		return tools.ScheduledStandingGrants(time.Unix(req.Admission.StandingGrantsAuthorizedAt, 0))
	}
	if runOrigin(ctx, req) == runOriginCron {
		return tools.ScheduledStandingGrants(req.StandingGrantsAuthorizedAt)
	}
	return tools.InteractiveStandingGrants()
}
