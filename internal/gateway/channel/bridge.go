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
	resp, err := b.gateway.Handle(ctx, uid, channel, content)
	if err != nil {
		return "", err
	}

	// 3. Handle synchronous result
	if !resp.IsStreaming {
		return resp.Content, nil
	}

	// 4. Handle streaming (aggregate for platforms that don't support streams, like basic WeChat)
	var fullText string
	for event := range resp.Stream {
		if event.Err != nil {
			return fullText, event.Err
		}
		fullText += event.Content
	}
	return fullText, nil
}

// Message represents a generic message structure for adapters.
type Message struct {
	Platform   string
	PlatformID string
	Channel    string
	Content    string
}
