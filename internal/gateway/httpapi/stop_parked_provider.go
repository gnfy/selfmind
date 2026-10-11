package httpapi

import (
	"context"
	"fmt"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

func (d *Server) requestActiveRunStop(ctx context.Context, identity *control.IdentityContext, active *activeRun) error {
	if active.RunID != "" {
		if err := d.Control.RequestRunCancel(context.WithoutCancel(ctx), identity.TenantID, active.RunID); err != nil {
			return err
		}
		d.ExecutionRuntime.YieldQueuedHostExecution(identity.TenantID, identity.PersonID, active.RunID)
	}
	if active.Cancel != nil {
		active.Cancel()
	}
	return nil
}

func (d *Server) stopParkedRun(ctx context.Context, identity *control.IdentityContext, runID string) (bool, string, error) {
	stopped, err := d.Control.CancelParkedProviderRun(ctx, identity.TenantID, identity.PersonID, runID)
	if err == nil && !stopped {
		stopped, err = d.Control.CancelParkedHumanRun(ctx, identity.TenantID, identity.PersonID, runID)
	}
	if err != nil {
		return true, "Could not stop parked work: " + err.Error(), err
	}
	if stopped {
		d.ExecutionRuntime.YieldQueuedHostExecution(identity.TenantID, identity.PersonID, runID)
		d.coordinator().drainQueue(identity)
		return true, fmt.Sprintf("Cancelled parked run %s and its pending continuation.", shortRunID(runID)), nil
	}
	return false, "", nil
}

// Bare stop names a single unambiguous wait. CLI ambiguity is session-local;
// IM has one entrance and retains the person-wide choice.
func (d *Server) stopSoleParkedRun(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, attention []control.AttentionItem) (bool, string) {
	ids, err := d.Control.ParkedProviderRunIDs(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		return true, "Could not inspect parked work: " + err.Error()
	}
	humanIDs, err := d.Control.ParkedHumanRunIDs(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		return true, "Could not inspect parked work: " + err.Error()
	}
	ids = append(ids, humanIDs...)
	if len(ids) == 0 {
		return false, ""
	}
	if req.Platform == "cli" {
		// A parked Run keeps its original foreground channel. A CLI window
		// without an exact selection cannot stop another window's work merely
		// because it is the person's only wait.
		own := ids[:0]
		for _, id := range ids {
			run, err := d.Control.GetRun(ctx, identity.TenantID, id)
			if err != nil {
				return true, "Could not inspect parked work: " + err.Error()
			}
			if run != nil && req.Channel != "" && run.Channel == req.Channel {
				own = append(own, id)
			}
		}
		ids = own
		if len(ids) == 0 {
			return false, ""
		}
	}
	seen := map[string]bool{}
	for _, item := range attention {
		if req.Platform == "cli" {
			run, err := d.Control.GetRun(ctx, identity.TenantID, item.RunID)
			if err != nil {
				return true, "Could not inspect parked work: " + err.Error()
			}
			if run == nil || req.Channel == "" || run.Channel != req.Channel {
				continue
			}
		}
		seen[item.RunID] = true
	}
	for _, id := range ids {
		seen[id] = true
	}
	if len(seen) > 1 {
		return true, "Several items need attention or are parked. Use /status to choose an exact run, then /stop <run_id>."
	}
	handled, reply, _ := d.stopParkedRun(ctx, identity, ids[0])
	return handled, reply
}
