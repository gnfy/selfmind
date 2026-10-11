package api

import (
	"errors"
	"fmt"
)

// ErrInboundUncertain means a durable ingress receipt may already have
// dispatched work. A channel adapter must keep that receipt for inspection
// and must not replay the message as a new effect.
var ErrInboundUncertain = errors.New("inbound processing outcome uncertain")

// ErrInboundHeld is an uncertain outcome the sender must hear about: the
// input took part of its effect, such as an answer that claimed its choice,
// but its request was not started. The adapter handles it as uncertain and
// shows the response error once; that text is written for the sender.
var ErrInboundHeld = fmt.Errorf("inbound input held after a partial effect: %w", ErrInboundUncertain)

// ErrInboundNotDispatched means the gateway refused the input before anything
// acted on it and its receipt, if one exists, is pending again. A channel
// adapter must keep the message redeliverable — never advance a sync cursor
// past it — and may show the sender the response error; a redelivery is
// dispatched at most once.
var ErrInboundNotDispatched = errors.New("inbound was not dispatched")

// ErrInboundRejected means the gateway refused the input for good before
// dispatch, for example a redelivered ID that no longer matches its saved
// receipt. Nothing ran and every redelivery would be refused again, so the
// adapter shows the response error once and moves past the message.
var ErrInboundRejected = errors.New("inbound was rejected")

// DurableInbound carries authenticated, normalized platform input to the
// gateway. The gateway owns identity binding, receipt, claim, and dispatch.
type DurableInbound struct {
	Request       MessageRequest
	MessageID     string
	RawPayload    []byte
	OwnerPersonID string
	// Essential reduces a RawPayload of this adapter's format to the fields
	// that identify the message, so a redelivery that only changed transport
	// fields still matches its receipt. Nil compares raw payloads byte for
	// byte.
	Essential func(raw []byte) ([]byte, error)
}
