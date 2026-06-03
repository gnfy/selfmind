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

type TelegramSender struct {
	Token  string
	Client *http.Client
}

func (s *TelegramSender) Send(ctx context.Context, msg Message) error {
	token := strings.TrimSpace(s.Token)
	if token == "" {
		return ErrNoSender
	}
	chatID := strings.TrimSpace(msg.Channel)
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
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token), bytes.NewReader(body))
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
