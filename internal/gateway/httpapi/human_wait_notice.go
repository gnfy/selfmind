package httpapi

import (
	"context"

	"selfmind/internal/control"
	"selfmind/internal/platform/log"
)

// emitHumanWaitEvent publishes one of the events that make PARKED work visible —
// an approval or clarification the person must answer before anything continues.
// It reports whether the live surface was actually informed.
//
// Two things went wrong on 2026-09-04 and both live here now. The event was
// emitted with its error discarded, so a broken invariant produced no error
// anywhere and only a missing prompt. And the notification fallback is
// suppressed while a CLI is attached, justified by "the live TUI already shows
// it" — a claim about the live path EXISTING, not about it SUCCEEDING. With the
// append failing silently and the push suppressed on its behalf, nobody was
// told through any channel: a production release sat parked for 21 minutes
// behind a spinner. The escrow sweep does eventually escalate to IM, but only
// once the person detaches or the request ages past the threshold, which is no
// help to someone sitting there watching.
//
// Callers must pass the result to the notification path so suppression is
// conditioned on the live surface having actually been informed.
func emitHumanWaitEvent(ctx context.Context, store *control.Store, event control.Event, idKey, idValue string) bool {
	if store == nil {
		return false
	}
	if _, err := store.AppendEvent(ctx, event); err != nil {
		log.Warn("failed to append a human-wait event; the person cannot see this from the live surface",
			"event", event.Type, idKey, idValue, "run_id", event.RunID, "error", err)
		return false
	}
	return true
}
