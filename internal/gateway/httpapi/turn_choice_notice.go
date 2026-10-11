package httpapi

import (
	"context"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/delivery"
)

// Asking which work owns an input is itself a durable human wait. Persist its
// notice after Main chooses to ask, before acknowledging the inbound message.
func (d *Server) enqueueTurnChoiceNotice(ctx context.Context, req api.MessageRequest, resp *api.MessageResponse) error {
	if resp.Choice == nil || resp.Identity == nil || resp.Error != "" ||
		req.Platform == "cli" || req.Platform == "eval" || req.Platform == "http" ||
		!d.Delivery.SupportsPlatform(req.Platform) {
		return nil
	}
	accepted, err := d.Delivery.Enqueue(ctx, delivery.Message{
		TenantID: resp.Identity.TenantID, PersonID: resp.Identity.PersonID,
		Platform: req.Platform, PlatformUserID: resp.Identity.PlatformUserID, Channel: req.Channel,
		Kind: delivery.KindTurnChoice, ChoiceID: resp.Choice.ID, Content: resp.Content,
		LogicalKey: "turn-choice:" + resp.Choice.ID,
	})
	resp.DeliveryEnqueued = accepted
	return err
}
