package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultTelegramAPIBaseURL = "https://api.telegram.org"

type TelegramSender struct {
	Token string
	// BaseURL overrides the Telegram API host (tests); empty = production API.
	BaseURL string
	Client  *http.Client
}

func (s *TelegramSender) Send(ctx context.Context, msg Message) error {
	token := strings.TrimSpace(s.Token)
	if token == "" {
		return ErrNoSender
	}
	chatID := strings.TrimSpace(msg.Channel)
	if chatID == "" {
		chatID = strings.TrimSpace(msg.PlatformUserID)
	}
	if chatID == "" {
		return fmt.Errorf("telegram channel/chat_id is required")
	}
	text := msg.Content
	if msg.PartTotal > 1 {
		text = fmt.Sprintf("[%d/%d]\n%s", msg.PartIndex, msg.PartTotal, text)
	}
	payload := map[string]interface{}{
		"chat_id": chatID,
		"text":    text,
	}
	// Approval notifications get native approve/reject buttons; the callback
	// data round-trips the approval id so the inbound callback_query handler can
	// respond without parsing message text. Buttons only go on the final part of
	// a split message so a long preview does not produce duplicate keyboards.
	if msg.Kind == KindApproval && strings.TrimSpace(msg.ApprovalID) != "" && (msg.PartTotal <= 1 || msg.PartIndex == msg.PartTotal) {
		payload["reply_markup"] = map[string]interface{}{
			"inline_keyboard": [][]map[string]string{
				{{"text": "✅ Approve", "callback_data": "approve:" + strings.TrimSpace(msg.ApprovalID)}},
				{{"text": "❌ Reject", "callback_data": "reject:" + strings.TrimSpace(msg.ApprovalID)}},
			},
		}
	}
	base := strings.TrimRight(strings.TrimSpace(s.BaseURL), "/")
	if base == "" {
		base = defaultTelegramAPIBaseURL
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/bot%s/sendMessage", base, token), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("telegram sendMessage failed: %s %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return nil
}
