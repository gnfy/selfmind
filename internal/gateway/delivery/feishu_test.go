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

type fakeFeishu struct {
	mu             sync.Mutex
	tokenHits      int
	sendHits       int
	lastReceiveID  string
	lastReceiveTyp string
	lastText       string
}

func (f *fakeFeishu) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/open-apis/auth/v3/tenant_access_token/internal", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.tokenHits++
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"code": 0, "tenant_access_token": "t-xyz", "expire": 7200})
	})
	mux.HandleFunc("/open-apis/im/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var in struct {
			ReceiveID string `json:"receive_id"`
			MsgType   string `json:"msg_type"`
			Content   string `json:"content"`
		}
		_ = json.Unmarshal(body, &in)
		var content struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal([]byte(in.Content), &content)
		f.mu.Lock()
		f.sendHits++
		f.lastReceiveID = in.ReceiveID
		f.lastReceiveTyp = r.URL.Query().Get("receive_id_type")
		f.lastText = content.Text
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"code": 0, "msg": "ok"})
	})
	return mux
}

func TestFeishuSenderSendsTextAndCachesToken(t *testing.T) {
	fake := &fakeFeishu{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	s := &FeishuSender{AppID: "app", AppSecret: "secret", BaseURL: srv.URL}
	for i := 0; i < 2; i++ {
		if err := s.Send(context.Background(), Message{Platform: "feishu", Channel: "oc_chat1", Content: "hi"}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if fake.tokenHits != 1 {
		t.Fatalf("expected token fetched once (cached), got %d", fake.tokenHits)
	}
	if fake.lastReceiveID != "oc_chat1" || fake.lastReceiveTyp != "chat_id" {
		t.Fatalf("expected chat_id routing, got id=%q type=%q", fake.lastReceiveID, fake.lastReceiveTyp)
	}
	if fake.lastText != "hi" {
		t.Fatalf("unexpected text %q", fake.lastText)
	}
}

func TestFeishuSenderRoutesOpenID(t *testing.T) {
	fake := &fakeFeishu{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	s := &FeishuSender{AppID: "app", AppSecret: "secret", BaseURL: srv.URL}
	if err := s.Send(context.Background(), Message{Platform: "feishu", PlatformUserID: "ou_user1", Content: "yo"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if fake.lastReceiveID != "ou_user1" || fake.lastReceiveTyp != "open_id" {
		t.Fatalf("expected open_id routing, got id=%q type=%q", fake.lastReceiveID, fake.lastReceiveTyp)
	}
}

func TestFeishuSenderRequiresCredentials(t *testing.T) {
	s := &FeishuSender{}
	if err := s.Send(context.Background(), Message{Platform: "feishu", Channel: "oc_x", Content: "y"}); err != ErrNoSender {
		t.Fatalf("expected ErrNoSender, got %v", err)
	}
}
