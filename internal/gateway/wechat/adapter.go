package wechat

import (
	"context"
	"fmt"

	"selfmind/internal/gateway/router"
)

// Adapter 微信消息适配器
// 负责接收微信平台消息、解析 openid、调用统一 Gateway 处理
type Adapter struct {
	gateway *router.Gateway
}

// NewAdapter 创建一个微信适配器
func NewAdapter(gw *router.Gateway) *Adapter {
	return &Adapter{gateway: gw}
}

// HandleMessage 处理微信消息
// openid: 微信用户 openid
// content: 消息内容
// 返回: 回复内容
func (a *Adapter) HandleMessage(openid, content string) (string, error) {
	ctx := context.Background()

	// 1. 解析 unified_uid（自动绑定或创建）
	unifiedUID, err := a.gateway.ResolveUID(ctx, "wechat", openid)
	if err != nil {
		return "", fmt.Errorf("resolve uid: %w", err)
	}

	// 2. 交给 Gateway 处理（意图分流 + 任务管理）
	resp, err := a.gateway.Handle(ctx, unifiedUID, "wechat", content)
	if err != nil {
		return "", fmt.Errorf("gateway handle: %w", err)
	}

	// 3. 处理流式输出聚合
	if !resp.IsStreaming {
		return resp.Content, nil
	}

	var fullText string
	for event := range resp.Stream {
		if event.Err != nil {
			return fullText, event.Err
		}
		fullText += event.Content
	}

	return fullText, nil
}

// HandleRawMessage 处理微信平台推送的原始 XML/JSON 消息
// 这是 HTTP handler 的包装，便于接入微信公众平台的 callback 模式
// 具体实现需要根据微信公众平台的加密模式和消息格式来解析
//
// 示例：微信服务器会 POST 到你的服务器，body 是 XML 格式：
// <xml>
//   <ToUserName><![CDATA[toUser]]></ToUserName>
//   <FromUserName><![CDATA[fromUser]]></FromUserName>
//   <MsgType><![CDATA[text]]></MsgType>
//   <Content><![CDATA[content]]></Content>
//   <MsgId>1234567890</MsgId>
// </xml>
//
// 实现时需要：
//  1. 验证微信签名（token、timestamp、nonce）
//  2. 解析 XML 提取 FromUserName (openid) 和 Content
//  3. 调用 HandleMessage(openid, content)
//  4. 返回符合微信格式的 XML 响应
func (a *Adapter) HandleRawMessage(body []byte) ([]byte, error) {
	// TODO: 实现 XML 解析 + 签名验证
	// 目前只是一个骨架，实际接入时需要：
	// - 解析微信 POST body 的 XML
	// - 验证 msg_signature
	// - 响应时需要构造 <xml><Content><![CDATA[回复内容]]></Content></xml>
	return []byte(""), fmt.Errorf("not implemented: use HandleMessage directly for now")
}

// BindPlatform 将微信 openid 绑定到已有的 unified_uid
// 用于将已有账号与微信关联的场景
func (a *Adapter) BindPlatform(ctx context.Context, openid, unifiedUID string) error {
	// Adapter 本身不直接操作 identityMapper，通过 gateway 代理
	return nil
}
