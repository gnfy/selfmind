package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/platform/log"
)

// defaultInboundSettleTimeout bounds how long one receipt transition waits.
const defaultInboundSettleTimeout = 10 * time.Second

// settleContext keeps receipt transitions alive after the caller goes away.
// The WeChat poll context is cancelled by Stop() exactly while a shutdown
// refuses input, and a webhook request ends with its client; a transition cut
// short would strand a proven refusal as dispatching or contradict what the
// sender was told. The bound limits waiting only: a claim, release,
// acceptance, or reason that reached the store runs to completion, so an
// error from one means it changed nothing.
func (d *Server) settleContext(ctx context.Context) (context.Context, context.CancelFunc) {
	bound := d.inboundSettleTimeout
	if bound <= 0 {
		bound = defaultInboundSettleTimeout
	}
	return context.WithTimeout(context.WithoutCancel(ctx), bound)
}

// ProcessDurableInbound is the gateway-owned ingress transaction boundary for
// authenticated platform adapters. An accepted receipt is idempotent; a
// dispatching receipt retains uncertainty instead of replaying effects. Input
// refused before admission reports api.ErrInboundNotDispatched with its receipt
// pending, so the adapter keeps the message redeliverable; a redelivery that
// conflicts with its receipt reports api.ErrInboundRejected.
func (d *Server) ProcessDurableInbound(ctx context.Context, inbound api.DurableInbound) (api.MessageResponse, int, error) {
	if d == nil || d.Control == nil || strings.TrimSpace(inbound.MessageID) == "" || len(inbound.RawPayload) == 0 {
		return api.MessageResponse{}, http.StatusServiceUnavailable, fmt.Errorf("durable inbound identity and payload are required")
	}
	req := inbound.Request
	if cliClaim(req.Platform) {
		// The CLI platform belongs to the person's own terminal on the local
		// control socket; no platform adapter speaks for it.
		return refuseInbound(nil, req.Platform, inbound.MessageID, refusalIdentity, fmt.Errorf("IM ingress cannot claim the CLI platform"))
	}
	tenant := d.tenantID(req.TenantID)
	if inbound.OwnerPersonID != "" {
		if _, err := d.Control.BindAccount(ctx, tenant, inbound.OwnerPersonID, req.Platform, req.PlatformUserID, req.DisplayName); err != nil {
			return refuseInbound(nil, req.Platform, inbound.MessageID, refusalIdentity, err)
		}
	}
	identity, err := d.Control.ResolveOrCreateAccount(ctx, tenant, req.Platform, req.PlatformUserID, req.DisplayName)
	if err != nil {
		return refuseInbound(nil, req.Platform, inbound.MessageID, refusalIdentity, err)
	}
	beginCtx, cancelBegin := d.settleContext(ctx)
	state, err := d.Control.BeginInboundMatching(beginCtx, req.Platform, inbound.MessageID, inbound.RawPayload, control.InboundOwner{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Preview: req.Content,
	}, inbound.Essential)
	cancelBegin()
	if errors.Is(err, control.ErrInboundConflict) {
		return rejectInbound(identity, req.Platform, inbound.MessageID, err)
	}
	if err != nil {
		return refuseInbound(identity, req.Platform, inbound.MessageID, refusalSave, err)
	}
	if state == control.InboundAccepted {
		return api.MessageResponse{}, http.StatusOK, nil
	}
	if state != control.InboundPending {
		return api.MessageResponse{}, http.StatusServiceUnavailable, fmt.Errorf("%s message %s: %w", req.Platform, inbound.MessageID, api.ErrInboundUncertain)
	}
	return d.dispatchInboundReceipt(ctx, identity, req.Platform, inbound.MessageID, req)
}

// dispatchInboundReceipt claims a pending receipt, runs its input through the
// gateway, and settles the receipt from what actually happened: accepted when
// handled, pending again when refused before admission, and dispatching when a
// failure may follow an effect. Only the claim admits one dispatcher, so
// concurrent redeliveries of a pending receipt run it once. Every settlement
// names the claim's generation and lands only while that claim holds the
// receipt. A receipt found pending again is redeliverable whoever released
// it, so a refusal still keeps its source; one that another claim holds or
// settled no longer speaks for this dispatch, so its outcome is held as
// uncertain. Each receipt transition gets its own bound; the turn in between
// may take far longer.
func (d *Server) dispatchInboundReceipt(ctx context.Context, identity *control.IdentityContext, platform, messageID string, req api.MessageRequest) (api.MessageResponse, int, error) {
	generation, receipt, err := d.claimInbound(ctx, platform, messageID)
	if err != nil {
		switch receipt.state {
		case control.InboundAccepted:
			return api.MessageResponse{}, http.StatusOK, nil
		case control.InboundDispatching:
			// A claim this delivery cannot show is its own holds the receipt: a
			// claim that reports an error has not committed, barring a failure
			// after its commit. The receipt stays held for inspection. Its
			// reason is the holder's to record, so this delivery's only fills
			// a reason the holder has not recorded, and the holder's replaces it.
			d.explainHeldInbound(ctx, platform, messageID, receipt.generation, "claim outcome unknown; this delivery did not dispatch it")
			log.Error("inbound claim failed while the receipt is claimed; receipt held for inspection", "platform", platform, "message_id", messageID, "claim_generation", receipt.generation, "error", err)
			return api.MessageResponse{}, http.StatusServiceUnavailable, fmt.Errorf("%s message %s claim outcome unknown: %w: %w", platform, messageID, api.ErrInboundUncertain, err)
		case control.InboundPending:
			return refuseInbound(identity, platform, messageID, refusalSave, err)
		default:
			return unreadableReceipt(platform, messageID, err)
		}
	}
	if generation == 0 {
		return api.MessageResponse{}, http.StatusServiceUnavailable, fmt.Errorf("%s message %s: %w", platform, messageID, api.ErrInboundUncertain)
	}
	resp, status, refusal := d.processMessage(ctx, req)
	switch {
	case refusal != nil && refusal.consumedChoice:
		reason := "choice answered, but its request was not saved (" + refusal.reason + ")"
		d.noteInbound(ctx, platform, messageID, generation, reason)
		log.Error("inbound answer claimed its choice but its request was not saved; receipt held", "platform", platform, "message_id", messageID, "reason", refusal.reason, "error", refusal.cause)
		return heldAnswer(identity, refusal.reason), http.StatusServiceUnavailable,
			fmt.Errorf("%s message %s answered a choice that was not saved: %w", platform, messageID, api.ErrInboundHeld)
	case refusal != nil:
		if err := d.releaseInbound(ctx, platform, messageID, generation, "not dispatched: "+refusal.reason); err != nil {
			receipt, readErr := d.rereadInbound(ctx, platform, messageID)
			switch {
			case readErr != nil:
				return unreadableReceipt(platform, messageID, errors.Join(err, readErr))
			case receipt.state == control.InboundPending:
				// Released, by this claim or a later one: whatever the
				// generation, the receipt is redeliverable and its next claim
				// admits one dispatcher, so the source keeps the message.
			case receipt.generation != generation:
				return claimLost(api.MessageResponse{Identity: identity}, platform, messageID, generation, err)
			default:
				// The refusal is proven, but a receipt that stays claimed reads
				// as uncertain everywhere else; never report it redeliverable.
				d.noteInbound(ctx, platform, messageID, generation, fmt.Sprintf("not dispatched (%s); receipt release failed", refusal.reason))
				log.Error("inbound refusal could not release its receipt; held for inspection", "platform", platform, "message_id", messageID, "reason", refusal.reason, "error", err)
				return api.MessageResponse{Identity: identity}, http.StatusServiceUnavailable,
					fmt.Errorf("%s message %s refused before admission, receipt still claimed: %w: %w", platform, messageID, api.ErrInboundUncertain, err)
			}
		}
		return refuseInbound(identity, platform, messageID, refusal.reason, refusal.cause)
	case status >= http.StatusInternalServerError:
		if resp.Choice != nil && !resp.DeliveryEnqueued {
			d.noteInbound(ctx, platform, messageID, generation, "choice saved, but its notice was not saved")
			return resp, status, fmt.Errorf("%s message %s saved a choice without its notice: %w", platform, messageID, api.ErrInboundHeld)
		}
		d.noteInbound(ctx, platform, messageID, generation, fmt.Sprintf("gateway returned HTTP %d", status))
		return resp, status, fmt.Errorf("%s message %s: %w", platform, messageID, api.ErrInboundUncertain)
	}
	if err := d.acceptInbound(ctx, platform, messageID, generation); err != nil {
		receipt, readErr := d.rereadInbound(ctx, platform, messageID)
		switch {
		case readErr == nil && receipt.generation != generation:
			// Another claim holds the receipt or settled it; an acceptance
			// found under it is not this dispatch's.
			return claimLost(resp, platform, messageID, generation, err)
		case readErr != nil || receipt.state != control.InboundAccepted:
			d.noteInbound(ctx, platform, messageID, generation, "handled, but its acceptance was not saved")
			return resp, http.StatusServiceUnavailable, fmt.Errorf("%s message %s acceptance not saved: %w: %w", platform, messageID, api.ErrInboundUncertain, err)
		}
		// The acceptance committed although it reported a failure.
	}
	return resp, status, nil
}

// receiptRead is what a reread finds: the receipt's state and the generation
// of its latest claim. The zero value is a receipt that could not be read.
type receiptRead struct {
	state      control.InboundState
	generation int64
}

// claimInbound claims a pending receipt within one bound and returns the
// claim's generation, 0 when another dispatcher holds the receipt. After a
// failed claim it rereads the receipt so the caller says only what the
// receipt shows; a receipt it cannot read leaves the read empty.
func (d *Server) claimInbound(ctx context.Context, platform, messageID string) (int64, receiptRead, error) {
	settleCtx, cancel := d.settleContext(ctx)
	generation, err := d.Control.ClaimInbound(settleCtx, platform, messageID)
	cancel()
	if err == nil {
		return generation, receiptRead{}, nil
	}
	receipt, readErr := d.rereadInbound(ctx, platform, messageID)
	if readErr != nil {
		return 0, receiptRead{}, errors.Join(err, readErr)
	}
	return 0, receipt, err
}

// rereadInbound reads a receipt after one of its transitions failed, within a
// bound of its own. Only the receipt says where that leaves the message: the
// transition may have found it released, held, or settled under another
// claim, and only a receipt still under this dispatch's generation reflects
// this dispatch's own transitions.
func (d *Server) rereadInbound(ctx context.Context, platform, messageID string) (receiptRead, error) {
	settleCtx, cancel := d.settleContext(ctx)
	defer cancel()
	state, generation, err := d.Control.InboundReceiptClaim(settleCtx, platform, messageID)
	return receiptRead{state: state, generation: generation}, err
}

// claimLost reports a dispatch whose receipt another claim holds or settled.
// The receipt no longer speaks for this dispatch, so nothing is recorded on
// it, and the outcome is held as uncertain like any unproven settlement —
// never a refusal that promises a retry.
func claimLost(resp api.MessageResponse, platform, messageID string, generation int64, cause error) (api.MessageResponse, int, error) {
	log.Error("inbound receipt is no longer held by this dispatch's claim; outcome held as uncertain",
		"platform", platform, "message_id", messageID, "claim_generation", generation, "error", cause)
	return resp, http.StatusServiceUnavailable,
		fmt.Errorf("%s message %s claim %d no longer holds its receipt: %w: %w", platform, messageID, generation, api.ErrInboundUncertain, cause)
}

// unreadableReceipt reports a failed transition whose receipt cannot be read.
// Whether the transition committed is unknown, so the sender is promised
// nothing: the source keeps the message, and its redelivery finds out from
// the receipt.
func unreadableReceipt(platform, messageID string, err error) (api.MessageResponse, int, error) {
	log.Error("inbound receipt transition failed and the receipt is unreadable; delivery will be retried", "platform", platform, "message_id", messageID, "error", err)
	return api.MessageResponse{}, http.StatusServiceUnavailable, fmt.Errorf("%s message %s receipt unreadable after a failed transition: %w", platform, messageID, err)
}

func (d *Server) releaseInbound(ctx context.Context, platform, messageID string, generation int64, reason string) error {
	settleCtx, cancel := d.settleContext(ctx)
	defer cancel()
	return d.Control.ReleaseInbound(settleCtx, platform, messageID, generation, reason)
}

func (d *Server) acceptInbound(ctx context.Context, platform, messageID string, generation int64) error {
	settleCtx, cancel := d.settleContext(ctx)
	defer cancel()
	return d.Control.AcceptInbound(settleCtx, platform, messageID, generation)
}

// noteInbound records why a held receipt is held, for /diag inbound, under the
// claim the reason belongs to. The reason is a short class, never the message
// or a raw internal error.
func (d *Server) noteInbound(ctx context.Context, platform, messageID string, generation int64, reason string) {
	settleCtx, cancel := d.settleContext(ctx)
	defer cancel()
	if err := d.Control.NoteInboundFailure(settleCtx, platform, messageID, generation, errors.New(reason)); err != nil {
		log.Warn("inbound receipt reason could not be recorded", "platform", platform, "message_id", messageID, "error", err)
	}
}

// explainHeldInbound records why this delivery did not dispatch a receipt that
// another claim holds, where that claim has recorded no reason of its own.
func (d *Server) explainHeldInbound(ctx context.Context, platform, messageID string, generation int64, reason string) {
	settleCtx, cancel := d.settleContext(ctx)
	defer cancel()
	if err := d.Control.NoteUnexplainedInbound(settleCtx, platform, messageID, generation, errors.New(reason)); err != nil {
		log.Warn("inbound receipt reason could not be recorded", "platform", platform, "message_id", messageID, "error", err)
	}
}

// heldAnswer reports an answer whose choice was claimed before its request
// could be saved. A redelivered answer would only find the choice used, so
// nothing here promises a retry.
func heldAnswer(identity *control.IdentityContext, reason string) api.MessageResponse {
	text := "Your answer was recorded, but its request was not started because " + reason +
		". Check /diag inbound, then send the request again if it is still needed."
	return api.MessageResponse{Identity: identity, Error: text, Turn: messageTurn("failed", "", "idle", "", "", text)}
}

// refuseInbound reports input that nothing acted on. Its source still holds
// the message — a sync cursor that did not move past it, or a webhook the
// platform redelivers after 503 — so the sender learns it will be retried and
// needs to resend only if it never starts; an eager resend would duplicate it.
// The chat sees only the short reason; the cause goes to the log.
func refuseInbound(identity *control.IdentityContext, platform, messageID, reason string, cause error) (api.MessageResponse, int, error) {
	if cause != nil {
		log.Warn("inbound input not dispatched", "platform", platform, "message_id", messageID, "reason", reason, "error", cause)
	}
	text := "This message was not started because " + reason + ". Nothing ran; it will be retried automatically. " +
		"If it still has not started once SelfMind is available again, send it again."
	return api.MessageResponse{Identity: identity, Error: text, Turn: messageTurn("failed", "", "idle", "", "", text)},
		http.StatusServiceUnavailable, fmt.Errorf("%s message %s: %w", platform, messageID, api.ErrInboundNotDispatched)
}

// rejectInbound reports a redelivered ID that no longer matches its saved
// receipt. No retry can admit it, so the sender is told once without a retry
// promise and the source is told to move on.
func rejectInbound(identity *control.IdentityContext, platform, messageID string, cause error) (api.MessageResponse, int, error) {
	log.Error("inbound redelivery conflicts with its receipt; message rejected", "platform", platform, "message_id", messageID, "error", cause)
	text := "This message was not started: it does not match an earlier delivery with the same ID, so it will not be retried. " +
		"Send it again if it is still needed."
	return api.MessageResponse{Identity: identity, Error: text, Turn: messageTurn("failed", "", "idle", "", "", text)},
		http.StatusConflict, fmt.Errorf("%s message %s: %w", platform, messageID, api.ErrInboundRejected)
}
