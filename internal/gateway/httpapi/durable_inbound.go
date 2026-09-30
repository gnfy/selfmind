package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

// ProcessDurableInbound is the gateway-owned ingress transaction boundary for
// authenticated platform adapters. An accepted receipt is idempotent; a
// dispatching receipt retains uncertainty instead of replaying effects.
func (d *Server) ProcessDurableInbound(ctx context.Context, inbound api.DurableInbound) (api.MessageResponse, int, error) {
	if d == nil || d.Control == nil || strings.TrimSpace(inbound.MessageID) == "" || len(inbound.RawPayload) == 0 {
		return api.MessageResponse{}, http.StatusServiceUnavailable, fmt.Errorf("durable inbound identity and payload are required")
	}
	req := inbound.Request
	tenant := d.tenantID(req.TenantID)
	if inbound.OwnerPersonID != "" {
		if _, err := d.Control.BindAccount(ctx, tenant, inbound.OwnerPersonID, req.Platform, req.PlatformUserID, req.DisplayName); err != nil {
			return api.MessageResponse{}, http.StatusServiceUnavailable, err
		}
	}
	identity, err := d.Control.ResolveOrCreateAccount(ctx, tenant, req.Platform, req.PlatformUserID, req.DisplayName)
	if err != nil {
		return api.MessageResponse{}, http.StatusServiceUnavailable, err
	}
	state, err := d.Control.BeginInbound(ctx, req.Platform, inbound.MessageID, inbound.RawPayload, control.InboundOwner{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Preview: req.Content,
	})
	if err != nil {
		return api.MessageResponse{}, http.StatusServiceUnavailable, err
	}
	if state == control.InboundAccepted {
		return api.MessageResponse{}, http.StatusOK, nil
	}
	if state != control.InboundPending {
		return api.MessageResponse{}, http.StatusServiceUnavailable, fmt.Errorf("%s message %s: %w", req.Platform, inbound.MessageID, api.ErrInboundUncertain)
	}
	claimed, err := d.Control.ClaimInbound(ctx, req.Platform, inbound.MessageID)
	if err != nil {
		return api.MessageResponse{}, http.StatusServiceUnavailable, err
	}
	if !claimed {
		return api.MessageResponse{}, http.StatusServiceUnavailable, fmt.Errorf("%s message %s: %w", req.Platform, inbound.MessageID, api.ErrInboundUncertain)
	}
	resp, status := d.ProcessMessage(ctx, req)
	if status >= http.StatusInternalServerError {
		_ = d.Control.NoteInboundFailure(ctx, req.Platform, inbound.MessageID, fmt.Errorf("gateway returned HTTP %d", status))
		return resp, status, fmt.Errorf("%s message %s: %w", req.Platform, inbound.MessageID, api.ErrInboundUncertain)
	}
	if err := d.Control.AcceptInbound(ctx, req.Platform, inbound.MessageID); err != nil {
		return resp, http.StatusServiceUnavailable, fmt.Errorf("%s message %s acceptance not saved: %w: %w", req.Platform, inbound.MessageID, api.ErrInboundUncertain, err)
	}
	return resp, status, nil
}
