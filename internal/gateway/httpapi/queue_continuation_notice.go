package httpapi

import (
	"context"
	"errors"
	"fmt"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/platform/log"
)

// handleObsoleteQueueClaim covers the change between the drain's last read
// and atomic child creation. Its notice owns delivery; the async finalizer
// must not overwrite a retained or retired row.
func (c *RunCoordinator) handleObsoleteQueueClaim(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest) bool {
	if req.QueueID == "" || req.QueueClaimToken == "" {
		return false
	}
	row, err := c.srv.Control.GetQueued(ctx, identity.TenantID, req.QueueID)
	if err != nil || row == nil || row.PersonID != identity.PersonID || row.ClaimToken != req.QueueClaimToken {
		return false
	}
	_, _, err = c.srv.Control.ResolveQueuedContinuation(ctx, *row)
	var obsolete *control.QueuedContinuationObsolete
	if !errors.As(err, &obsolete) {
		return false
	}
	if !c.retireObsoleteContinuation(ctx, identity, *row, obsolete) {
		if _, releaseErr := c.returnUnboundQueueClaim(ctx, identity.TenantID, row.ID, req.QueueClaimToken); releaseErr != nil {
			log.Warn("gateway: release changed continuation claim failed", "queue_id", row.ID, "error", releaseErr)
		}
	}
	return true
}

// retireObsoleteContinuation records why this wakeup cannot dispatch before
// settling the unbound row. The human decision and unfinished Run are retained.
// Failed event/outbox writes leave the row available for a later bounded pass.
func (c *RunCoordinator) retireObsoleteContinuation(ctx context.Context, identity *control.IdentityContext, row control.QueuedTask, cause *control.QueuedContinuationObsolete) bool {
	if row.RunID != "" {
		return false
	} // a bound Run owns execution and recovery
	inspectRunID := row.ReplyToRunID
	if cause.ChildRunID != "" {
		inspectRunID = cause.ChildRunID
	}
	content := fmt.Sprintf("A queued continuation did not start: use /resume %s to inspect remaining work. Its saved decision was retained; no action was executed by this continuation. Reason: %s.", inspectRunID, cause)
	key := "queue-continuation-obsolete:" + row.ID
	// Subscribe before committing the notice. The durable stream maps it to
	// the same CLI notice on live delivery and replay, even if settlement fails.
	c.srv.events()
	_, err := c.srv.Control.AppendEvent(ctx, control.Event{TenantID: row.TenantID, PersonID: row.PersonID,
		TaskID: row.TaskID, RunID: row.ReplyToRunID, Channel: row.Channel, Type: "queue.continuation_obsolete",
		Visibility: "task", IdempotencyKey: key,
		Payload: mustJSON(map[string]string{"queue_id": row.ID, "approval_id": row.ApprovalID,
			"parent_run_id": row.ReplyToRunID, "child_run_id": cause.ChildRunID, "message": content})})
	if err != nil {
		log.Warn("gateway: could not record an obsolete continuation; its queue row was retained", "queue_id", row.ID, "error", err)
		return false
	}
	// Presence does not prove delivery: an idempotent retry may find a new CLI
	// whose initial cursor skipped this already-saved event. Keep the one keyed
	// outbox fallback instead of silencing both surfaces in that window.
	if !c.queueNoticePush(ctx, identity, row, delivery.Message{TenantID: row.TenantID, PersonID: row.PersonID,
		TaskID: row.TaskID, RunID: row.ReplyToRunID, Kind: delivery.KindRecovery, Content: content, LogicalKey: key}, false) {
		return false
	}
	changed, err := c.srv.Control.FailUnboundQueued(ctx, row.TenantID, row.ID, row.ClaimToken)
	if err != nil {
		log.Warn("gateway: could not settle an obsolete continuation; its queue row was retained", "queue_id", row.ID, "error", err)
		return false
	}
	return changed
}
