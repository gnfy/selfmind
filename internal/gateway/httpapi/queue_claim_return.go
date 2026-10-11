package httpapi

import (
	"context"
	"fmt"
	"sync"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/platform/log"
	"selfmind/internal/tools"
)

type unboundQueueReturn struct {
	mu       sync.Mutex
	tenantID string
	queueID  string
	token    string
}

type queuedNotice struct {
	identity control.IdentityContext
	row      control.QueuedTask
	message  delivery.Message
}

// returnUnboundQueueClaim remembers a confirmed non-dispatch cleanup until
// its exact-token write succeeds. It never discovers or reaps expired claims:
// a live worker may still be preparing an unbound Run. A daemon crash uses the
// existing boot recovery of the durable row, not this process-local retry.
func (c *RunCoordinator) returnUnboundQueueClaim(ctx context.Context, tenantID, queueID, token string) (bool, error) {
	if queueID == "" || token == "" {
		return false, fmt.Errorf("queue id and claim token are required")
	}
	key := tenantID + "\x00" + queueID + "\x00" + token
	value, _ := c.pendingQueueReturns.LoadOrStore(key, &unboundQueueReturn{tenantID: tenantID, queueID: queueID, token: token})
	return c.tryUnboundQueueReturn(ctx, key, value.(*unboundQueueReturn))
}

func (c *RunCoordinator) tryUnboundQueueReturn(ctx context.Context, key string, pending *unboundQueueReturn) (bool, error) {
	pending.mu.Lock()
	defer pending.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	changed, err := c.srv.Control.RequeueUnboundClaim(ctx, pending.tenantID, pending.queueID, pending.token)
	if err == nil {
		// A CAS miss is settled too: a bound Run, cancellation, or a newer
		// claim owns the row now. Never undo it on a later tick.
		c.pendingQueueReturns.CompareAndDelete(key, pending)
	}
	return changed, err
}

// One bounded cleanup attempt per scheduler pass, even when a returned claim
// is the only pending work and ListDueQueuedRoutes cannot see it yet.
func (c *RunCoordinator) retryUnboundQueueReturns(ctx context.Context) {
	c.pendingQueueReturns.Range(func(key, value any) bool {
		if ctx.Err() != nil {
			return false
		}
		pending := value.(*unboundQueueReturn)
		if _, err := c.tryUnboundQueueReturn(ctx, key.(string), pending); err != nil {
			log.Warn("gateway: unbound queue claim return remains pending", "queue_id", pending.queueID, "error", err)
		}
		return true
	})
}

// Before a Run exists there is no atomic Run finalizer or saved final answer.
// Keep the original input retryable and tell its session what happened; an IM
// account gets the same bounded notice through the existing keyed outbox.
func (c *RunCoordinator) retainQueuedStartupFailure(identity *control.IdentityContext, req api.MessageRequest, cause string) {
	ctx := context.Background()
	changed, err := c.returnUnboundQueueClaim(ctx, identity.TenantID, req.QueueID, req.QueueClaimToken)
	if err != nil {
		log.Warn("gateway: failed queued startup retained its claim for a later return", "queue_id", req.QueueID, "error", err)
		// A failed write proves neither non-dispatch nor continued ownership.
		// Do not publish a claim about a bound or reassigned row.
		row, readErr := c.srv.Control.GetQueued(ctx, identity.TenantID, req.QueueID)
		if readErr != nil || row == nil || row.PersonID != identity.PersonID || row.RunID != "" || row.ClaimToken != req.QueueClaimToken {
			return
		}
	} else if !changed {
		return
	}
	content := "Queued work could not start: " + truncate(tools.RedactSensitive(cause), 300) +
		"\nNo task action was dispatched. The request was retained for a later retry. Use /queue to inspect or drop it."
	log.Warn("gateway: queued startup failed before Run creation; input retained", "queue_id", req.QueueID, "error", cause)
	c.srv.events().publish(api.RunEvent{TenantID: identity.TenantID, PersonID: identity.PersonID,
		Channel: req.Channel, Type: "queue.start_failed", Durability: api.EventEphemeral, CreatedAt: time.Now(),
		Payload: mustJSON(map[string]string{"queue_id": req.QueueID, "message": content, "kind": "warning"})})
	row := control.QueuedTask{ID: req.QueueID, TenantID: identity.TenantID, PersonID: identity.PersonID, Channel: req.Channel, Platform: req.Platform}
	notice := &queuedNotice{identity: *identity, row: row,
		message: delivery.Message{TenantID: identity.TenantID, PersonID: identity.PersonID,
			Kind: delivery.KindRecovery, Content: content, LogicalKey: "queue-start-failed:" + req.QueueID}}
	value, _ := c.pendingQueueNotices.LoadOrStore(notice.message.LogicalKey, notice)
	c.tryQueuedNotice(ctx, value.(*queuedNotice))
}

func (c *RunCoordinator) tryQueuedNotice(ctx context.Context, notice *queuedNotice) {
	if c.queueNoticePush(ctx, &notice.identity, notice.row, notice.message, false) {
		c.pendingQueueNotices.CompareAndDelete(notice.message.LogicalKey, notice)
	}
}

func (c *RunCoordinator) retryQueuedNotices(ctx context.Context) {
	c.pendingQueueNotices.Range(func(_, value any) bool {
		if ctx.Err() != nil {
			return false
		}
		c.tryQueuedNotice(ctx, value.(*queuedNotice))
		return true
	})
}
