package telegram

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"selfmind/internal/gateway/router"
)

const defaultTelegramAPIBaseURL = "https://api.telegram.org"

// Adapter Telegram 消息适配器
// 负责接收 Telegram 消息、解析 user id、调用统一 Gateway 处理
type Adapter struct {
	gateway       *router.Gateway
	token         string
	webhookURL    string
	webhookSecret string
	apiBaseURL    string
	client        *http.Client
	// approvalHandler responds to inline approve/reject button taps. It is
	// injected by the wiring layer because the adapter must never own approval
	// lifecycle state (docs/identity-continuity.md): the handler routes the
	// decision into the gateway/control-store respond path and returns the
	// user-facing result text.
	approvalHandler ApprovalHandler
	// Long polling state
	longPollMu   sync.Mutex
	longPollStop chan struct{}
	longPollDone chan struct{}
}

// ApprovalHandler resolves an inline-button approval decision for the Telegram
// user. userID is Telegram's numeric user id; decision is "approved" or
// "rejected"; approvalID is the apr_ id round-tripped through callback_data.
// The returned text is shown to the user (callback toast / edited message).
type ApprovalHandler func(ctx context.Context, userID int64, decision, approvalID string) (string, error)

// NewAdapter 创建一个 Telegram 适配器
func NewAdapter(gw *router.Gateway, token, webhookURL string) *Adapter {
	return &Adapter{
		gateway:    gw,
		token:      token,
		webhookURL: webhookURL,
		apiBaseURL: defaultTelegramAPIBaseURL,
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

	reply, _, err := router.AggregateFinalResponse(resp)
	return reply, err
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
				if u.CallbackQuery != nil {
					cb := u.CallbackQuery
					go a.handleCallbackQuery(context.Background(), cb)
					offset = u.UpdateID + 1
					continue
				}
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
					a.sendWorkingNotice(chatID)
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
	url := fmt.Sprintf("%s?offset=%d&timeout=25", a.apiURL("getUpdates"), offset)
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
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	url := a.apiURL("sendMessage")

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

func (a *Adapter) sendWorkingNotice(chatID int64) {
	notice := router.WorkingNotice("telegram")
	if notice == "" {
		return
	}
	_ = a.sendMessage(chatID, notice)
}

func (a *Adapter) apiURL(method string) string {
	base := strings.TrimRight(strings.TrimSpace(a.apiBaseURL), "/")
	if base == "" {
		base = defaultTelegramAPIBaseURL
	}
	return fmt.Sprintf("%s/bot%s/%s", base, a.token, method)
}

// SendText 发送文本消息（公开方法，供外部调用）
func (a *Adapter) SendText(chatID int64, text string) error {
	return a.sendMessage(chatID, text)
}

// SetWebhookSecret configures the secret token Telegram must echo back in the
// X-Telegram-Bot-Api-Secret-Token header on every webhook delivery. When set,
// WebhookHandler rejects requests that do not present the matching token,
// preventing third parties from spoofing updates to the public webhook URL.
func (a *Adapter) SetWebhookSecret(secret string) {
	a.webhookSecret = strings.TrimSpace(secret)
}

// SetWebhook 注册 webhook（用于生产环境）
func (a *Adapter) SetWebhook(ctx context.Context, webhookURL string) error {
	url := a.apiURL("setWebhook")
	payload := map[string]string{"url": webhookURL}
	if a.webhookSecret != "" {
		payload["secret_token"] = a.webhookSecret
	}
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

	// Reject spoofed deliveries: when a secret token is configured, Telegram
	// echoes it in this header. A constant-time compare avoids leaking the
	// token through response timing.
	if a.webhookSecret != "" {
		got := r.Header.Get("X-Telegram-Bot-Api-Secret-Token")
		if subtle.ConstantTimeCompare([]byte(got), []byte(a.webhookSecret)) != 1 {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
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

	if update.CallbackQuery != nil {
		cb := update.CallbackQuery
		go a.handleCallbackQuery(context.Background(), cb)
		w.WriteHeader(http.StatusOK)
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
		a.sendWorkingNotice(chatID)
		reply, err := a.HandleMessage(chatID, userID, username, text)
		if err != nil {
			return
		}
		_ = a.sendMessage(chatID, reply)
	}()

	w.WriteHeader(http.StatusOK)
}

// SetApprovalHandler installs the responder for inline approve/reject buttons.
// Without a handler, button taps get a hint to reply with the text command.
func (a *Adapter) SetApprovalHandler(handler ApprovalHandler) {
	a.approvalHandler = handler
}

// handleCallbackQuery processes an inline-button tap: parse the approval
// decision from callback_data, route it through the injected approval
// responder, acknowledge the tap (answerCallbackQuery), and append the outcome
// to the original message so the chat history shows what was decided.
func (a *Adapter) handleCallbackQuery(ctx context.Context, cb *CallbackQuery) {
	if cb == nil {
		return
	}
	decision, approvalID := parseApprovalCallbackData(cb.Data)
	if decision == "" {
		_ = a.answerCallbackQuery(ctx, cb.ID, "")
		return
	}
	if a.approvalHandler == nil {
		_ = a.answerCallbackQuery(ctx, cb.ID, "Approval buttons are not wired; reply /approve "+approvalID)
		return
	}
	var userID int64
	if cb.From != nil {
		userID = cb.From.ID
	}
	result, err := a.approvalHandler(ctx, userID, decision, approvalID)
	if err != nil {
		_ = a.answerCallbackQuery(ctx, cb.ID, "Failed: "+err.Error())
		return
	}
	suffix := "— ✅ approved by you"
	toast := "Approved"
	if decision == "rejected" {
		suffix = "— ❌ rejected"
		toast = "Rejected"
	}
	if result != "" {
		toast = result
	}
	_ = a.answerCallbackQuery(ctx, cb.ID, toast)
	if cb.Message != nil && cb.Message.Chat.ID != 0 && cb.Message.MessageID != 0 {
		// Editing also removes the inline keyboard (reply_markup omitted), so a
		// decided approval cannot be tapped twice from the same message.
		_ = a.editMessageText(ctx, cb.Message.Chat.ID, cb.Message.MessageID, strings.TrimSpace(cb.Message.Text+"\n"+suffix))
	}
}

// parseApprovalCallbackData splits "approve:<id>" / "reject:<id>" callback
// data into a store decision and the approval reference; ("", "") for any
// other payload.
func parseApprovalCallbackData(data string) (decision, approvalID string) {
	data = strings.TrimSpace(data)
	switch {
	case strings.HasPrefix(data, "approve:"):
		return "approved", strings.TrimSpace(strings.TrimPrefix(data, "approve:"))
	case strings.HasPrefix(data, "reject:"):
		return "rejected", strings.TrimSpace(strings.TrimPrefix(data, "reject:"))
	default:
		return "", ""
	}
}

// answerCallbackQuery acknowledges an inline-button tap so the Telegram client
// stops showing its loading spinner; text becomes a small toast when present.
func (a *Adapter) answerCallbackQuery(ctx context.Context, callbackQueryID, text string) error {
	payload := map[string]interface{}{
		"callback_query_id": callbackQueryID,
	}
	if strings.TrimSpace(text) != "" {
		payload["text"] = text
	}
	return a.postJSON(ctx, "answerCallbackQuery", payload)
}

func (a *Adapter) editMessageText(ctx context.Context, chatID, messageID int64, text string) error {
	return a.postJSON(ctx, "editMessageText", map[string]interface{}{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
	})
}

func (a *Adapter) postJSON(ctx context.Context, method string, payload map[string]interface{}) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.apiURL(method), bytes.NewReader(body))
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
		return fmt.Errorf("telegram %s error: %d %s", method, resp.StatusCode, string(respBody))
	}
	return nil
}

// Update 表示 Telegram API 的 update 对象
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
}

// CallbackQuery 表示 Telegram API 的 callback_query 对象（内联按钮点击）
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    *User    `json:"from,omitempty"`
	Message *Message `json:"message,omitempty"`
	Data    string   `json:"data,omitempty"`
}

// Message 表示 Telegram API 的 message 对象
type Message struct {
	MessageID int64 `json:"message_id"`
	Chat      struct {
		ID int64 `json:"id"`
	} `json:"chat"`
	From *User  `json:"from,omitempty"`
	Text string `json:"text,omitempty"`
	Date int64  `json:"date"`
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
