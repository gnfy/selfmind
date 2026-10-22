package httpapi

import (
	"context"
	"time"

	"selfmind/internal/control"
)

type queueResourceBlock struct{ Kind, RunID string }

// This view calls the admission predicate; it does not invent a second
// lifecycle status or persist a reason that can become stale.
func (d *Server) queueWaitReason(ctx context.Context, identity *control.IdentityContext, q control.QueuedTask, active []*activeRun) string {
	if q.AdmissionError != nil {
		return "The saved execution authority cannot be read; use /queue drop to remove this request."
	}
	if q.NotBefore.After(time.Now()) {
		return "Waiting until " + q.NotBefore.Format(time.RFC3339)
	}
	if d.IsDraining() {
		return "Waiting for the daemon to finish its safe restart."
	}
	if !d.modelReadyForWork() {
		return "Waiting for model readiness; open selfmind model."
	}
	reservations, err := d.Control.ListRunRootReservations(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		return "Execution occupancy cannot be inspected yet; use /status."
	}
	if blocker := queuedResourceBlocker(q, active, reservations); blocker != nil {
		reason := "Another Run is using overlapping writable directories"
		switch blocker.Kind {
		case "session":
			reason = "This CLI session already has an executing Run"
		case "continuation":
			reason = "The exact continuation's parent is still executing"
		case "unknown_scope":
			reason = "Execution roots are not yet known to be independent"
		}
		if blocker.RunID != "" {
			return reason + " (" + shortRunID(blocker.RunID) + "). Use /resume to view work or /status " + blocker.RunID + " for its exact state"
		}
		return reason + "; use /status to inspect admission"
	}
	if len(active) >= d.coordinator().activeLimit || len(reservations) >= d.coordinator().activeLimit {
		return "All active work slots are occupied; use /status to view them"
	}
	if q.ReplyToRunID != "" {
		return "Waiting for the exact continuation's admission checks; use /status " + q.ReplyToRunID
	}
	return "Waiting for earlier input from this session or the next queue pass; use /queue to inspect requests"
}
