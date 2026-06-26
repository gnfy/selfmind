package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const defaultFeishuBaseURL = "https://open.feishu.cn"

// FeishuSender delivers outbound messages to Feishu / Lark via the IM message
// API (open-apis/im/v1/messages). It caches the app-level tenant_access_token
// and refreshes it on expiry or an auth errcode.
//
// The recipient is resolved from the delivery Message: a Channel that looks
// like a chat id ("oc_...") is sent with receive_id_type=chat_id; otherwise the
// PlatformUserID open id ("ou_...") is used with receive_id_type=open_id. This
// matches what the inbound webhook parser records (chat_id as Channel, open_id
// as PlatformUserID).
type FeishuSender struct {
	AppID     string
	AppSecret string
	BaseURL   string // defaults to https://open.feishu.cn
	Client    *http.Client

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

func (s *FeishuSender) httpClient() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (s *FeishuSender) baseURL() string {
	if b := strings.TrimRight(strings.TrimSpace(s.BaseURL), "/"); b != "" {
		return b
	}
	return defaultFeishuBaseURL
}

func (s *FeishuSender) Send(ctx context.Context, msg Message) error {
	if s == nil || strings.TrimSpace(s.AppID) == "" || strings.TrimSpace(s.AppSecret) == "" {
		return ErrNoSender
	}
	receiveID, receiveType := feishuReceiver(msg)
	if receiveID == "" {
		return fmt.Errorf("feishu receive_id (chat_id or open_id) is required")
	}
	text := msg.Content
	if msg.PartTotal > 1 {
		text = fmt.Sprintf("[%d/%d]\n%s", msg.PartIndex, msg.PartTotal, text)
	}
	if err := s.sendText(ctx, receiveID, receiveType, text, false); err != nil {
		if isFeishuTokenError(err) {
			return s.sendText(ctx, receiveID, receiveType, text, true)
		}
		return err
	}
	return nil
}

// feishuReceiver picks the receive id and its type. Feishu chat ids start with
// "oc_" and open ids with "ou_"; fall back to chat_id for an unknown channel.
func feishuReceiver(msg Message) (string, string) {
	channel := strings.TrimSpace(msg.Channel)
	user := strings.TrimSpace(msg.PlatformUserID)
	switch {
	case strings.HasPrefix(channel, "oc_"):
		return channel, "chat_id"
	case strings.HasPrefix(user, "ou_"):
		return user, "open_id"
	case channel != "":
		return channel, "chat_id"
	case user != "":
		return user, "open_id"
	default:
		return "", ""
	}
}

func (s *FeishuSender) sendText(ctx context.Context, receiveID, receiveType, text string, forceRefresh bool) error {
	token, err := s.tenantAccessToken(ctx, forceRefresh)
	if err != nil {
		return err
	}
	// content is a JSON-encoded string per the Feishu text message schema.
	contentJSON, _ := json.Marshal(map[string]string{"text": text})
	payload := map[string]interface{}{
		"receive_id": receiveID,
		"msg_type":   "text",
		"content":    string(contentJSON),
	}
	body, _ := json.Marshal(payload)
	endpoint := fmt.Sprintf("%s/open-apis/im/v1/messages?receive_id_type=%s", s.baseURL(), url.QueryEscape(receiveType))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("feishu send failed: %s %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var out feishuAPIResult
	if err := json.Unmarshal(data, &out); err != nil {
		return fmt.Errorf("feishu send: decode response: %w", err)
	}
	if out.Code != 0 {
		return &feishuError{Code: out.Code, Msg: out.Msg}
	}
	return nil
}

// tenantAccessToken returns a cached tenant_access_token, fetching a fresh one
// when empty/expired or when a refresh is forced. Guarded by mu so concurrent
// sends share a single token and a single refresh.
func (s *FeishuSender) tenantAccessToken(ctx context.Context, forceRefresh bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !forceRefresh && s.token != "" && time.Now().Before(s.tokenExp) {
		return s.token, nil
	}
	body, _ := json.Marshal(map[string]string{
		"app_id":     strings.TrimSpace(s.AppID),
		"app_secret": strings.TrimSpace(s.AppSecret),
	})
	endpoint := s.baseURL() + "/open-apis/auth/v3/tenant_access_token/internal"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("feishu token fetch failed: %s", resp.Status)
	}
	var out struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("feishu token decode: %w", err)
	}
	if out.Code != 0 || strings.TrimSpace(out.TenantAccessToken) == "" {
		return "", &feishuError{Code: out.Code, Msg: out.Msg}
	}
	ttl := out.Expire
	if ttl <= 0 {
		ttl = 7200
	}
	skew := 300
	if ttl <= skew {
		skew = ttl / 2
	}
	s.token = out.TenantAccessToken
	s.tokenExp = time.Now().Add(time.Duration(ttl-skew) * time.Second)
	return s.token, nil
}

type feishuAPIResult struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

type feishuError struct {
	Code int
	Msg  string
}

func (e *feishuError) Error() string {
	return fmt.Sprintf("feishu api error %d: %s", e.Code, e.Msg)
}

// isFeishuTokenError reports whether err is a token failure worth one
// forced-refresh retry (invalid/expired tenant_access_token).
func isFeishuTokenError(err error) bool {
	fe, ok := err.(*feishuError)
	if !ok {
		return false
	}
	switch fe.Code {
	case 99991663, 99991661, 99991664, 99991665:
		return true
	default:
		return false
	}
}
