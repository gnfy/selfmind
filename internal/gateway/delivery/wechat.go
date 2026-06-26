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

const defaultWechatBaseURL = "https://api.weixin.qq.com"

// WechatSender delivers outbound messages to a WeChat Official Account (公众号)
// via the customer-service message API (cgi-bin/message/custom/send).
//
// WeChat Official Accounts cannot push arbitrary messages: custom/send only
// works inside the 48h service window after a user interacts with the account.
// That matches the agent reply flow — the user just messaged us — so async run
// results and notifications can be pushed back. The sender caches the
// app-level access_token and refreshes it on expiry or an auth errcode.
type WechatSender struct {
	AppID     string
	AppSecret string
	BaseURL   string // defaults to https://api.weixin.qq.com
	Client    *http.Client

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

func (s *WechatSender) httpClient() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (s *WechatSender) baseURL() string {
	if b := strings.TrimRight(strings.TrimSpace(s.BaseURL), "/"); b != "" {
		return b
	}
	return defaultWechatBaseURL
}

func (s *WechatSender) Send(ctx context.Context, msg Message) error {
	if s == nil || strings.TrimSpace(s.AppID) == "" || strings.TrimSpace(s.AppSecret) == "" {
		return ErrNoSender
	}
	// For WeChat the recipient is the openid, carried as PlatformUserID; fall
	// back to Channel for callers that only set the conversation id.
	openid := strings.TrimSpace(msg.PlatformUserID)
	if openid == "" {
		openid = strings.TrimSpace(msg.Channel)
	}
	if openid == "" {
		return fmt.Errorf("wechat openid (platform_user_id) is required")
	}
	text := msg.Content
	if msg.PartTotal > 1 {
		text = fmt.Sprintf("[%d/%d]\n%s", msg.PartIndex, msg.PartTotal, text)
	}
	// Try once; if the token was rejected, refresh it and retry exactly once.
	if err := s.sendCustomText(ctx, openid, text, false); err != nil {
		if isWechatTokenError(err) {
			return s.sendCustomText(ctx, openid, text, true)
		}
		return err
	}
	return nil
}

func (s *WechatSender) sendCustomText(ctx context.Context, openid, text string, forceRefresh bool) error {
	token, err := s.accessToken(ctx, forceRefresh)
	if err != nil {
		return err
	}
	payload := map[string]interface{}{
		"touser":  openid,
		"msgtype": "text",
		"text":    map[string]string{"content": text},
	}
	body, _ := json.Marshal(payload)
	endpoint := fmt.Sprintf("%s/cgi-bin/message/custom/send?access_token=%s", s.baseURL(), url.QueryEscape(token))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("wechat custom/send failed: %s %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var out wechatAPIResult
	if err := json.Unmarshal(data, &out); err != nil {
		return fmt.Errorf("wechat custom/send: decode response: %w", err)
	}
	if out.ErrCode != 0 {
		return &wechatError{Code: out.ErrCode, Msg: out.ErrMsg}
	}
	return nil
}

// accessToken returns a cached app access_token, fetching a fresh one when the
// cache is empty/expired or a refresh is forced. Guarded by mu so concurrent
// sends share a single token and a single refresh.
func (s *WechatSender) accessToken(ctx context.Context, forceRefresh bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !forceRefresh && s.token != "" && time.Now().Before(s.tokenExp) {
		return s.token, nil
	}
	endpoint := fmt.Sprintf("%s/cgi-bin/token?grant_type=client_credential&appid=%s&secret=%s",
		s.baseURL(), url.QueryEscape(strings.TrimSpace(s.AppID)), url.QueryEscape(strings.TrimSpace(s.AppSecret)))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("wechat token fetch failed: %s", resp.Status)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		ErrCode     int    `json:"errcode"`
		ErrMsg      string `json:"errmsg"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("wechat token decode: %w", err)
	}
	if out.ErrCode != 0 || strings.TrimSpace(out.AccessToken) == "" {
		return "", &wechatError{Code: out.ErrCode, Msg: out.ErrMsg}
	}
	ttl := out.ExpiresIn
	if ttl <= 0 {
		ttl = 7200
	}
	// Refresh a little early so a near-expiry token is not used mid-request.
	skew := 300
	if ttl <= skew {
		skew = ttl / 2
	}
	s.token = out.AccessToken
	s.tokenExp = time.Now().Add(time.Duration(ttl-skew) * time.Second)
	return s.token, nil
}

type wechatAPIResult struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

type wechatError struct {
	Code int
	Msg  string
}

func (e *wechatError) Error() string {
	return fmt.Sprintf("wechat api error %d: %s", e.Code, e.Msg)
}

// isWechatTokenError reports whether err is an access_token failure worth one
// forced-refresh retry (invalid credential / invalid or expired token).
func isWechatTokenError(err error) bool {
	we, ok := err.(*wechatError)
	if !ok {
		return false
	}
	switch we.Code {
	case 40001, 40014, 42001:
		return true
	default:
		return false
	}
}
