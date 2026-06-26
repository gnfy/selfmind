package delivery

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

type fakeQQ struct {
	mu        sync.Mutex
	tokenHits int
	lastPath  string
	lastAuth  string
	lastUnion string
	lastBody  map[string]interface{}
}

func (f *fakeQQ) apiHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var b map[string]interface{}
		_ = json.Unmarshal(body, &b)
		f.mu.Lock()
		f.lastPath = r.URL.Path
		f.lastAuth = r.Header.Get("Authorization")
		f.lastUnion = r.Header.Get("X-Union-Appid")
		f.lastBody = b
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "m1"})
	})
}

func (f *fakeQQ) tokenHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.tokenHits++
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"access_token": "qq-tok", "expires_in": "7200"})
	})
}

func newQQSender(t *testing.T, fake *fakeQQ) (*QQSender, func()) {
	t.Helper()
	api := httptest.NewServer(fake.apiHandler())
	tok := httptest.NewServer(fake.tokenHandler())
	s := &QQSender{AppID: "appid", Secret: "secret", BaseURL: api.URL, TokenURL: tok.URL}
	return s, func() { api.Close(); tok.Close() }
}

func TestQQSenderRoutesGroup(t *testing.T) {
	fake := &fakeQQ{}
	s, done := newQQSender(t, fake)
	defer done()

	if err := s.Send(context.Background(), Message{Platform: "qq", Channel: "group:g123", Content: "hello"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if fake.lastPath != "/v2/groups/g123/messages" {
		t.Fatalf("unexpected path %q", fake.lastPath)
	}
	if fake.lastAuth != "QQBot qq-tok" || fake.lastUnion != "appid" {
		t.Fatalf("unexpected auth headers: auth=%q union=%q", fake.lastAuth, fake.lastUnion)
	}
	if fake.lastBody["content"] != "hello" {
		t.Fatalf("unexpected content %v", fake.lastBody["content"])
	}
}

func TestQQSenderRoutesC2CAndChannel(t *testing.T) {
	fake := &fakeQQ{}
	s, done := newQQSender(t, fake)
	defer done()

	if err := s.Send(context.Background(), Message{Platform: "qq", Channel: "c2c:u9", Content: "hi"}); err != nil {
		t.Fatalf("c2c send: %v", err)
	}
	if fake.lastPath != "/v2/users/u9/messages" {
		t.Fatalf("unexpected c2c path %q", fake.lastPath)
	}
	if err := s.Send(context.Background(), Message{Platform: "qq", Channel: "channel:ch7", Content: "hi"}); err != nil {
		t.Fatalf("channel send: %v", err)
	}
	if fake.lastPath != "/channels/ch7/messages" {
		t.Fatalf("unexpected channel path %q", fake.lastPath)
	}
	// token fetched once and reused across all sends
	if fake.tokenHits != 1 {
		t.Fatalf("expected token cached (1 fetch), got %d", fake.tokenHits)
	}
}

func TestQQSenderRequiresCredentials(t *testing.T) {
	s := &QQSender{}
	if err := s.Send(context.Background(), Message{Platform: "qq", Channel: "group:g", Content: "y"}); err != ErrNoSender {
		t.Fatalf("expected ErrNoSender, got %v", err)
	}
}
