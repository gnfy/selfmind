package channel

import (
	"context"
	"fmt"
	"selfmind/internal/gateway/router"
)

// Bridge acts as the glue between Platform Adapters and the Gateway.
// It mimics the Python Hermes platform management.
type Bridge struct {
	gateway *router.Gateway
}

func NewBridge(gw *router.Gateway) *Bridge {
	return &Bridge{gateway: gw}
}

// HandleInbound handles a message from an external platform.
// It resolves the identity and routes through the Gateway.
func (b *Bridge) HandleInbound(ctx context.Context, platform, platformID, channel, content string) (string, error) {
	// 1. Resolve UID
	uid, err := b.gateway.ResolveUID(ctx, platform, platformID)
	if err != nil {
		return "", fmt.Errorf("identity resolution failed: %w", err)
	}

	// 2. Call Gateway
	resp, err := b.gateway.HandleWithEvents(ctx, uid, channel, content)
	if err != nil {
		return "", err
	}

	reply, _, err := router.AggregateFinalResponse(resp)
	return reply, err
}

// Message represents a generic message structure for adapters.
type Message struct {
	Platform   string
	PlatformID string
	Channel    string
	Content    string
}
