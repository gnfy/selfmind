package httpapi

// Per-person task queueing (G1+G2). When a run is already active, genuinely new
// work is enqueued and auto-started when the active run finishes, instead of
// being rejected as "busy". The queue is durable (control.task_queue) so a
// queued task pending at restart still runs via the boot drain.

import (
	"context"
	"fmt"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/platform/log"
)

// enqueueBehindActive stores a new task behind the person's active run and
// returns an honest, conversational acceptance. "N ahead" counts the running
// task (1) plus items already queued before this one.
func (d *Server) enqueueBehindActive(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest) api.MessageResponse {
	if d == nil || d.Control == nil || identity == nil {
		return api.MessageResponse{Identity: identity, Error: "queue is not available", Turn: messageTurn("failed", "", "", "", "", "queue is not available")}
	}
	ahead, _ := d.Control.CountQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
	ahead++ // include the currently running task
	_, err := d.Control.EnqueueQueued(ctx, control.QueuedTask{
		TenantID:       identity.TenantID,
		PersonID:       identity.PersonID,
		Channel:        req.Channel,
		Platform:       req.Platform,
		PlatformUserID: req.PlatformUserID,
		Content:        req.Content,
		ApprovalMode:   req.ApprovalMode,
		WorkspaceID:    req.WorkspaceID,
		ExecutionRoots: req.ExecutionRoots,
	})
	if err != nil {
		return api.MessageResponse{Identity: identity, Error: err.Error(), Turn: messageTurn("failed", "", "", "", "", err.Error())}
	}
	content := fmt.Sprintf("Queued behind the running task (%d ahead). I'll start it when the current one finishes.", ahead)
	return api.MessageResponse{
		Identity: identity,
		Content:  content,
		Accepted: true,
		Turn:     messageTurn("queued", "queued", "running", "", "", content),
	}
}

// enqueueDuringModelChange accepts new work while the daemon is draining for a
// confirmed model transaction. The boot drain will start it with the new
// frozen route; rejecting it would make the safe switch look like downtime.
func (d *Server) enqueueDuringModelChange(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest) api.MessageResponse {
	if d == nil || d.Control == nil || identity == nil {
		return api.MessageResponse{Identity: identity, Error: "queue is not available", Turn: messageTurn("failed", "", "draining", "", "", "queue is not available")}
	}
	ahead, _ := d.Control.CountQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
	if d.coordinator().currentActive(identity.PersonID) != nil {
		ahead++
	}
	_, err := d.Control.EnqueueQueued(ctx, control.QueuedTask{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		Channel: req.Channel, Platform: req.Platform, PlatformUserID: req.PlatformUserID,
		Content: req.Content, ApprovalMode: req.ApprovalMode, WorkspaceID: req.WorkspaceID,
		ExecutionRoots: req.ExecutionRoots,
	})
	if err != nil {
		return api.MessageResponse{Identity: identity, Error: err.Error(), Turn: messageTurn("failed", "", "draining", "", "", err.Error())}
	}
	content := fmt.Sprintf("Queued for the model change (%d ahead). It will start after SelfMind applies the new model.", ahead)
	return api.MessageResponse{
		Identity: identity, Content: content, Accepted: true,
		Turn: messageTurn("queued", "queued", "draining", "", "", content),
	}
}

// DrainQueuedAtBoot resumes queued work after a gateway restart. The
// gateway.lock flock guarantees this is the only daemon on control.db, so any
// row left 'started' was mid-launch when the previous daemon died and never ran
// — requeue those first, then kick one drain per person with pending work (the
// drain chain handles the rest, one at a time per person).
func (d *Server) DrainQueuedAtBoot(ctx context.Context) {
	// Steering mailbox recovery (P0-A) runs FIRST so guidance orphaned by the
	// previous daemon lands in the queue before this drain launches anything:
	// live rows inside the replay window defer into task-pinned queued work,
	// stale rows expire.
	if d != nil && d.Control != nil {
		if deferred, expired, err := d.Control.RecoverSteeringAtBoot(ctx); err != nil {
			log.Warn("gateway: steering mailbox boot recovery failed", "error", err)
		} else if deferred+expired > 0 {
			log.Info("gateway: recovered steering mailbox", "deferred", deferred, "expired", expired)
		}
		// Prune resolved tool-ledger history (P0-B); uncertain dispatched rows
		// are correctness state and are never pruned here.
		if pruned, err := d.Control.PruneToolLedger(ctx, 0); err != nil {
			log.Warn("gateway: tool ledger prune failed", "error", err)
		} else if pruned > 0 {
			log.Info("gateway: pruned resolved tool ledger rows", "count", pruned)
		}
	}
	if d == nil || d.Control == nil {
		return
	}
	// A decision can be committed immediately before the old daemon dies. Repair
	// that decision->continuation edge before listing the queue so the recovered
	// work participates in the same one-run-per-person boot drain.
	d.recoverApprovalContinuations(ctx, false)
	requeued, dropped, _ := d.Control.RequeueStartedQueued(ctx)
	if dropped > 0 {
		log.Warn("gateway: dropped queued tasks that exhausted their restart budget", "dropped", dropped, "requeued", requeued)
	}
	rows, err := d.Control.ListAllQueued(ctx, control.QueueStatusQueued)
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, q := range rows {
		key := q.TenantID + "|" + q.PersonID
		if seen[key] {
			continue
		}
		seen[key] = true
		identity := d.routeIdentityForPerson(ctx, q.TenantID, q.PersonID, q.Channel, q.Platform, nil)
		d.coordinator().drainQueue(identity)
	}
}
