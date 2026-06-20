package wechat

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"sort"
	"strings"
	"time"

	"selfmind/internal/gateway/router"
)

type Adapter struct {
	gateway *router.Gateway
}

func NewAdapter(gw *router.Gateway) *Adapter {
	return &Adapter{gateway: gw}
}

func (a *Adapter) HandleMessage(openid, content string) (string, error) {
	ctx := context.Background()
	unifiedUID, err := a.gateway.ResolveUID(ctx, "wechat", openid)
	if err != nil {
		return "", fmt.Errorf("resolve uid: %w", err)
	}
	resp, err := a.gateway.HandleWithEvents(ctx, unifiedUID, "wechat", content)
	if err != nil {
		return "", fmt.Errorf("gateway handle: %w", err)
	}
	reply, _, err := router.AggregateFinalResponse(resp)
	return reply, err
}

func (a *Adapter) HandleRawMessage(body []byte) ([]byte, error) {
	var msg inboundTextMessage
	if err := xml.Unmarshal(body, &msg); err != nil {
		return nil, fmt.Errorf("parse wechat xml: %w", err)
	}
	if strings.TrimSpace(msg.FromUserName) == "" {
		return nil, fmt.Errorf("missing FromUserName")
	}
	if strings.ToLower(strings.TrimSpace(msg.MsgType)) != "text" {
		return formatTextResponse(msg.FromUserName, msg.ToUserName, "SelfMind currently supports text messages only."), nil
	}
	reply, err := a.HandleMessage(msg.FromUserName, msg.Content)
	if err != nil {
		return nil, err
	}
	return formatTextResponse(msg.FromUserName, msg.ToUserName, reply), nil
}

func (a *Adapter) BindPlatform(ctx context.Context, openid, unifiedUID string) error {
	return nil
}

type inboundTextMessage struct {
	XMLName      xml.Name `xml:"xml"`
	ToUserName   string   `xml:"ToUserName"`
	FromUserName string   `xml:"FromUserName"`
	CreateTime   int64    `xml:"CreateTime"`
	MsgType      string   `xml:"MsgType"`
	Content      string   `xml:"Content"`
	MsgID        string   `xml:"MsgId"`
}

type outboundTextMessage struct {
	XMLName      xml.Name `xml:"xml"`
	ToUserName   cdata    `xml:"ToUserName"`
	FromUserName cdata    `xml:"FromUserName"`
	CreateTime   int64    `xml:"CreateTime"`
	MsgType      cdata    `xml:"MsgType"`
	Content      cdata    `xml:"Content"`
}

type cdata struct {
	Value string `xml:",cdata"`
}

func formatTextResponse(toUser, fromUser, content string) []byte {
	msg := outboundTextMessage{
		ToUserName:   cdata{Value: toUser},
		FromUserName: cdata{Value: fromUser},
		CreateTime:   time.Now().Unix(),
		MsgType:      cdata{Value: "text"},
		Content:      cdata{Value: content},
	}
	data, _ := xml.Marshal(msg)
	return data
}

func VerifySignature(token, timestamp, nonce, signature string) bool {
	parts := []string{token, timestamp, nonce}
	sort.Strings(parts)
	sum := sha1.Sum([]byte(strings.Join(parts, "")))
	return strings.EqualFold(hex.EncodeToString(sum[:]), strings.TrimSpace(signature))
}
