package delivery

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeWechat is a minimal stand-in for the WeChat Official Account API used to
// exercise WechatSender token caching, payload shape, and refresh-on-expiry.
type fakeWechat struct {
	mu         sync.Mutex
	tokenHits  int
	sendHits   int
	lastToUser string
	lastText   string
	// expireFirstToken makes custom/send reject the first access_token once
	// with errcode 42001, forcing a refresh + retry.
	expireFirstToken bool
	rejectedOnce     bool
}

func (f *fakeWechat) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/cgi-bin/token", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.tokenHits++
		tok := "tok-" + itoa(f.tokenHits)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": tok,
			"expires_in":   7200,
		})
	})
	mux.HandleFunc("/cgi-bin/message/custom/send", func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("access_token")
		f.mu.Lock()
		f.sendHits++
		if f.expireFirstToken && !f.rejectedOnce {
			f.rejectedOnce = true
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"errcode": 42001, "errmsg": "access_token expired"})
			return
		}
		f.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		var in struct {
			ToUser string `json:"touser"`
			Text   struct {
				Content string `json:"content"`
			} `json:"text"`
		}
		_ = json.Unmarshal(body, &in)
		f.mu.Lock()
		f.lastToUser = in.ToUser
		f.lastText = in.Text.Content
		f.mu.Unlock()
		if strings.TrimSpace(token) == "" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"errcode": 41001, "errmsg": "missing token"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"errcode": 0, "errmsg": "ok"})
	})
	return mux
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestWechatSenderSendsCustomTextAndCachesToken(t *testing.T) {
	fake := &fakeWechat{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	s := &WechatSender{AppID: "app", AppSecret: "secret", BaseURL: srv.URL}

	for i := 0; i < 2; i++ {
		if err := s.Send(context.Background(), Message{Platform: "wechat", PlatformUserID: "openid-1", Content: "hello"}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	if fake.tokenHits != 1 {
		t.Fatalf("expected token fetched once (cached), got %d", fake.tokenHits)
	}
	if fake.sendHits != 2 {
		t.Fatalf("expected 2 sends, got %d", fake.sendHits)
	}
	if fake.lastToUser != "openid-1" || fake.lastText != "hello" {
		t.Fatalf("unexpected payload: touser=%q text=%q", fake.lastToUser, fake.lastText)
	}
}

func TestWechatSenderRefreshesTokenOnExpiry(t *testing.T) {
	fake := &fakeWechat{expireFirstToken: true}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	s := &WechatSender{AppID: "app", AppSecret: "secret", BaseURL: srv.URL}
	if err := s.Send(context.Background(), Message{Platform: "wechat", PlatformUserID: "openid-1", Content: "hi"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if fake.tokenHits != 2 {
		t.Fatalf("expected token fetched twice (initial + forced refresh), got %d", fake.tokenHits)
	}
	if fake.lastText != "hi" {
		t.Fatalf("expected message delivered after refresh, got %q", fake.lastText)
	}
}

func TestWechatSenderRequiresCredentials(t *testing.T) {
	s := &WechatSender{}
	if err := s.Send(context.Background(), Message{Platform: "wechat", PlatformUserID: "x", Content: "y"}); err != ErrNoSender {
		t.Fatalf("expected ErrNoSender without credentials, got %v", err)
	}
}
