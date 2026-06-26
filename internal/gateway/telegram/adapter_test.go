package telegram

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"selfmind/internal/gateway/router"
)

func TestSendWorkingNoticeSendsSingleTelegramMessage(t *testing.T) {
	var gotText string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bottest-token/sendMessage" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		gotText, _ = payload["text"].(string)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	adapter := NewAdapter(nil, "test-token", "")
	adapter.apiBaseURL = server.URL
	adapter.client = server.Client()
	adapter.sendWorkingNotice(123)

	if gotText != router.WorkingNotice("telegram") {
		t.Fatalf("text = %q", gotText)
	}
}

func TestWebhookHandlerRejectsBadSecret(t *testing.T) {
	adapter := NewAdapter(nil, "test-token", "")
	adapter.SetWebhookSecret("expected-secret")

	body := strings.NewReader(`{"update_id":1,"message":{"text":"hi","chat":{"id":1},"from":{"id":1}}}`)
	req := httptest.NewRequest(http.MethodPost, "/telegram/webhook", body)
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "wrong-secret")
	rec := httptest.NewRecorder()

	adapter.WebhookHandler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestWebhookHandlerAcceptsGoodSecret(t *testing.T) {
	adapter := NewAdapter(nil, "test-token", "")
	adapter.SetWebhookSecret("expected-secret")

	// Empty-text message: handler returns 200 without dispatching to the gateway.
	body := strings.NewReader(`{"update_id":1,"message":{"text":"","chat":{"id":1},"from":{"id":1}}}`)
	req := httptest.NewRequest(http.MethodPost, "/telegram/webhook", body)
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "expected-secret")
	rec := httptest.NewRecorder()

	adapter.WebhookHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
