package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"selfmind/internal/gateway/router"
)

// Adapter Telegram 消息适配器
// 负责接收 Telegram 消息、解析 user id、调用统一 Gateway 处理
type Adapter struct {
	gateway   *router.Gateway
	token     string
	webhookURL string
	client    *http.Client
	// Long polling state
	longPollMu   sync.Mutex
	longPollStop chan struct{}
	longPollDone chan struct{}
}

// NewAdapter 创建一个 Telegram 适配器
func NewAdapter(gw *router.Gateway, token, webhookURL string) *Adapter {
	return &Adapter{
		gateway:     gw,
		token:       token,
		webhookURL: webhookURL,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		longPollStop: make(chan struct{}),
		longPollDone: make(chan struct{}),
	}
}

// HandleMessage 处理用户消息
// chatID: Telegram chat ID
// content: 消息内容
// 返回: 回复内容
func (a *Adapter) HandleMessage(chatID int64, userID int64, username, content string) (string, error) {
	ctx := context.Background()

	// Build a unique identity for this Telegram user
	platformID := fmt.Sprintf("telegram:%d", userID)

	// 1. 解析 unified_uid（自动绑定或创建）
	unifiedUID, err := a.gateway.ResolveUID(ctx, "telegram", platformID)
	if err != nil {
		return "", fmt.Errorf("resolve uid: %w", err)
	}

	// 2. 交给 Gateway 处理（意图分流 + 任务管理）
	resp, err := a.gateway.Handle(ctx, unifiedUID, "telegram", content)
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

// StartLongPolling 启动长轮询模式接收 Telegram 更新
// 这会在后台 goroutine 中运行，直到 StopLongPolling 被调用
func (a *Adapter) StartLongPolling(ctx context.Context) error {
	a.longPollMu.Lock()
	if a.longPollStop != nil {
		close(a.longPollStop)
	}
	a.longPollStop = make(chan struct{})
	a.longPollDone = make(chan struct{})
	a.longPollMu.Unlock()

	go func() {
		offset := int64(0)
		for {
			select {
			case <-a.longPollStop:
				close(a.longPollDone)
				return
			default:
			}

			updates, err := a.getUpdates(offset)
			if err != nil {
				time.Sleep(1 * time.Second)
				continue
			}

			for _, u := range updates {
				if u.Message == nil {
					continue
				}
				msg := u.Message
				text := strings.TrimSpace(msg.Text)
				if text == "" {
					continue
				}

				userID := msg.From.ID
				chatID := msg.Chat.ID
				username := ""
				if msg.From.Username != "" {
					username = msg.From.Username
				}

				go func(chatID int64, userID int64, username, text string) {
					reply, err := a.HandleMessage(chatID, userID, username, text)
					if err != nil {
						return
					}
					// Send the reply
					_ = a.sendMessage(chatID, reply)
				}(chatID, userID, username, text)

				offset = u.UpdateID + 1
			}
		}
	}()

	return nil
}

// StopLongPolling 停止长轮询
func (a *Adapter) StopLongPolling() {
	a.longPollMu.Lock()
	defer a.longPollMu.Unlock()
	if a.longPollStop != nil {
		close(a.longPollStop)
	}
}

// getUpdates 获取 Telegram 更新（长轮询）
func (a *Adapter) getUpdates(offset int64) ([]Update, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=25", a.token, offset)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(context.Background())

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram API error: status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result struct {
		OK     bool     `json:"ok"`
		Result []Update `json:"result"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	return result.Result, nil
}

// sendMessage 发送消息到 Telegram
func (a *Adapter) sendMessage(chatID int64, text string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", a.token)

	payload := map[string]interface{}{
		"chat_id": chatID,
		"text":    text,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telegram sendMessage error: %d %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// SendText 发送文本消息（公开方法，供外部调用）
func (a *Adapter) SendText(chatID int64, text string) error {
	return a.sendMessage(chatID, text)
}

// SetWebhook 注册 webhook（用于生产环境）
func (a *Adapter) SetWebhook(ctx context.Context, webhookURL string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/setWebhook", a.token)
	payload := map[string]string{"url": webhookURL}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("setWebhook failed: status %d", resp.StatusCode)
	}
	return nil
}

// WebhookHandler 处理 Telegram webhook 请求
// 用法: http.HandleFunc("/telegram/webhook", adapter.WebhookHandler)
func (a *Adapter) WebhookHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	var update Update
	if err := json.Unmarshal(body, &update); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	if update.Message == nil || update.Message.Text == "" {
		w.WriteHeader(http.StatusOK)
		return
	}

	msg := update.Message
	userID := msg.From.ID
	chatID := msg.Chat.ID
	username := ""
	if msg.From.Username != "" {
		username = msg.From.Username
	}
	text := strings.TrimSpace(msg.Text)

	go func() {
		reply, err := a.HandleMessage(chatID, userID, username, text)
		if err != nil {
			return
		}
		_ = a.sendMessage(chatID, reply)
	}()

	w.WriteHeader(http.StatusOK)
}

// Update 表示 Telegram API 的 update 对象
type Update struct {
	UpdateID int64  `json:"update_id"`
	Message  *Message `json:"message,omitempty"`
}

// Message 表示 Telegram API 的 message 对象
type Message struct {
	MessageID int64  `json:"message_id"`
	Chat     struct {
		ID int64 `json:"id"`
	} `json:"chat"`
	From    *User  `json:"from,omitempty"`
	Text    string `json:"text,omitempty"`
	Date    int64  `json:"date"`
}

// User 表示 Telegram API 的 user 对象
type User struct {
	ID           int64  `json:"id"`
	IsBot        bool   `json:"is_bot"`
	FirstName    string `json:"first_name"`
	LastName     string `json:"last_name,omitempty"`
	Username     string `json:"username,omitempty"`
	LanguageCode string `json:"language_code,omitempty"`
}
