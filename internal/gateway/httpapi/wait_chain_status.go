package httpapi

import (
	"context"

	"selfmind/internal/control"
)

// waitingRunDisplayLeaf is a read-only projection. It follows only durable
// child edges, never the newest Run in a Thread. The caller keeps the original
// control ID and explains which descendant supplies the displayed state.
func (d *Server) waitingRunDisplayLeaf(ctx context.Context, identity *control.IdentityContext, run *control.Run) (*control.Run, error) {
	switch run.Status {
	case "waiting_user", "waiting_external", "waiting_finalization", "blocked", "interrupted", "verification_partial":
	default:
		return run, nil
	}
	line, err := d.Control.ResumeLineRunIDs(ctx, identity.TenantID, run.ID)
	if err != nil {
		return nil, err
	}
	current := run
	found := false
	for _, id := range line {
		if id == run.ID {
			found = true
			continue
		}
		if !found {
			continue
		}
		child, err := d.Control.GetRun(ctx, identity.TenantID, id)
		if err != nil {
			return nil, err
		}
		if child == nil || child.PersonID != identity.PersonID || child.ExecutionClass == "coordination" || child.ResumesRunID != current.ID {
			break
		}
		current = child
	}
	return current, nil
}
