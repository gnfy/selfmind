package api

import "errors"

// ErrInboundUncertain means a durable ingress receipt may already have
// dispatched work. A channel adapter must keep that receipt for inspection
// and must not replay the message as a new effect.
var ErrInboundUncertain = errors.New("inbound processing outcome uncertain")

// DurableInbound carries authenticated, normalized platform input to the
// gateway. The gateway owns identity binding, receipt, claim, and dispatch.
type DurableInbound struct {
	Request       MessageRequest
	MessageID     string
	RawPayload    []byte
	OwnerPersonID string
}
