package delivery

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
)

const (
	defaultQQAPIBaseURL = "https://api.sgroup.qq.com"
	defaultQQTokenURL   = "https://bots.qq.com/app/getAppAccessToken"
)

// QQSender delivers outbound messages to a QQ official bot (QQ频道 / 群 / C2C).
//
// QQ separates token issuance (bots.qq.com) from the message API
// (api.sgroup.qq.com). The sender caches the app access token and refreshes it
// on expiry or an auth code. The target endpoint is chosen from the delivery
// Channel prefix, which the inbound webhook parser sets:
//   - "channel:<id>" -> guild sub-channel message
//   - "group:<openid>" -> group message
//   - "c2c:<openid>"   -> single-user message
//
// Note: these are ACTIVE (push) sends with no inbound msg_id. QQ rate-limits or
// gates active messages per bot policy; threading the inbound msg_id for free
// passive replies is a follow-up.
type QQSender struct {
	AppID    string
	Secret   string
	BaseURL  string // message API base, defaults to https://api.sgroup.qq.com
	TokenURL string // token endpoint, defaults to https://bots.qq.com/app/getAppAccessToken
	Client   *http.Client

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

func (s *QQSender) httpClient() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (s *QQSender) apiBase() string {
	if b := strings.TrimRight(strings.TrimSpace(s.BaseURL), "/"); b != "" {
		return b
	}
	return defaultQQAPIBaseURL
}

func (s *QQSender) tokenURL() string {
	if u := strings.TrimSpace(s.TokenURL); u != "" {
		return u
	}
	return defaultQQTokenURL
}

func (s *QQSender) Send(ctx context.Context, msg Message) error {
	if s == nil || strings.TrimSpace(s.AppID) == "" || strings.TrimSpace(s.Secret) == "" {
		return ErrNoSender
	}
	endpoint, payload, err := s.route(msg)
	if err != nil {
		return err
	}
	if err := s.post(ctx, endpoint, payload, false); err != nil {
		if isQQTokenError(err) {
			return s.post(ctx, endpoint, payload, true)
		}
		return err
	}
	return nil
}

// route resolves the QQ message endpoint and body from the delivery target.
func (s *QQSender) route(msg Message) (string, map[string]interface{}, error) {
	text := msg.Content
	if msg.PartTotal > 1 {
		text = fmt.Sprintf("[%d/%d]\n%s", msg.PartIndex, msg.PartTotal, text)
	}
	target := strings.TrimSpace(msg.Channel)
	if target == "" {
		target = strings.TrimSpace(msg.PlatformUserID)
	}
	kind, id := splitQQTarget(target)
	if id == "" {
		return "", nil, fmt.Errorf("qq delivery target is empty")
	}
	base := s.apiBase()
	switch kind {
	case "group":
		return fmt.Sprintf("%s/v2/groups/%s/messages", base, id), map[string]interface{}{"content": text, "msg_type": 0}, nil
	case "c2c", "user":
		return fmt.Sprintf("%s/v2/users/%s/messages", base, id), map[string]interface{}{"content": text, "msg_type": 0}, nil
	default: // channel (guild sub-channel)
		return fmt.Sprintf("%s/channels/%s/messages", base, id), map[string]interface{}{"content": text}, nil
	}
}

// splitQQTarget parses a "kind:id" target; an unprefixed value is treated as a
// guild channel id for backward compatibility.
func splitQQTarget(target string) (string, string) {
	if i := strings.Index(target, ":"); i > 0 {
		return strings.ToLower(target[:i]), strings.TrimSpace(target[i+1:])
	}
	return "channel", target
}

func (s *QQSender) post(ctx context.Context, endpoint string, payload map[string]interface{}, forceRefresh bool) error {
	token, err := s.accessToken(ctx, forceRefresh)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "QQBot "+token)
	req.Header.Set("X-Union-Appid", strings.TrimSpace(s.AppID))
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	var out qqAPIResult
	_ = json.Unmarshal(data, &out)
	if out.Code != 0 {
		return &qqError{Code: out.Code, Msg: firstNonEmptyQQ(out.Message, strings.TrimSpace(string(data)))}
	}
	return fmt.Errorf("qq send failed: %s %s", resp.Status, strings.TrimSpace(string(data)))
}

// accessToken returns a cached app access token, fetching a fresh one when
// empty/expired or when a refresh is forced.
func (s *QQSender) accessToken(ctx context.Context, forceRefresh bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !forceRefresh && s.token != "" && time.Now().Before(s.tokenExp) {
		return s.token, nil
	}
	body, _ := json.Marshal(map[string]string{
		"appId":        strings.TrimSpace(s.AppID),
		"clientSecret": strings.TrimSpace(s.Secret),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL(), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("qq token fetch failed: %s", resp.Status)
	}
	// expires_in may be a number or a numeric string depending on the endpoint.
	var out struct {
		AccessToken string      `json:"access_token"`
		ExpiresIn   json.Number `json:"expires_in"`
		Code        int         `json:"code"`
		Message     string      `json:"message"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("qq token decode: %w", err)
	}
	if strings.TrimSpace(out.AccessToken) == "" {
		return "", &qqError{Code: out.Code, Msg: firstNonEmptyQQ(out.Message, "empty access_token")}
	}
	ttl := 7200
	if n, err := out.ExpiresIn.Int64(); err == nil && n > 0 {
		ttl = int(n)
	}
	skew := 300
	if ttl <= skew {
		skew = ttl / 2
	}
	s.token = out.AccessToken
	s.tokenExp = time.Now().Add(time.Duration(ttl-skew) * time.Second)
	return s.token, nil
}

type qqAPIResult struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type qqError struct {
	Code int
	Msg  string
}

func (e *qqError) Error() string {
	return fmt.Sprintf("qq api error %d: %s", e.Code, e.Msg)
}

// isQQTokenError reports whether err is an auth-token failure worth one
// forced-refresh retry. 11244/11251/401-family codes indicate a bad/expired token.
func isQQTokenError(err error) bool {
	qe, ok := err.(*qqError)
	if !ok {
		return false
	}
	switch qe.Code {
	case 11244, 11251, 401, 100007:
		return true
	default:
		return false
	}
}

func firstNonEmptyQQ(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
