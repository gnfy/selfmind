package wechat

import (
	"context"
	"selfmind/internal/gateway/channel"
	"selfmind/internal/platform/log"
)

// Adapter implements the WeChat platform logic.
type Adapter struct {
	bridge *channel.Bridge
}

func NewAdapter(bridge *channel.Bridge) *Adapter {
	return &Adapter{bridge: bridge}
}

// HandleRequest is called by the web server (e.g. Gin) when a WeChat message arrives.
func (a *Adapter) HandleRequest(ctx context.Context, openID, content string) (string, error) {
	// WeChat is typically a single 'wechat' channel, but could be 'wechat_mp', 'wechat_corp'.
	return a.bridge.HandleInbound(ctx, "wechat", openID, "wechat", content)
}

// SendMessage is used for asynchronous replies (Customer Service API).
func (a *Adapter) SendMessage(ctx context.Context, openID, text string) error {
	log.Info("wechat message sent", "openID", openID, "text", text)
	return nil
}
