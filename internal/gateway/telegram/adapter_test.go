package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestWebhookHandlerRoutesCallbackQueryToApprovalHandler(t *testing.T) {
	var mu sync.Mutex
	answered := false
	edited := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		mu.Lock()
		switch r.URL.Path {
		case "/bottest-token/answerCallbackQuery":
			answered = true
		case "/bottest-token/editMessageText":
			edited, _ = payload["text"].(string)
		}
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	adapter := NewAdapter(nil, "test-token", "")
	adapter.apiBaseURL = server.URL
	adapter.client = server.Client()

	var gotUserID int64
	var gotDecision, gotApprovalID string
	adapter.SetApprovalHandler(func(ctx context.Context, userID int64, decision, approvalID string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		gotUserID, gotDecision, gotApprovalID = userID, decision, approvalID
		return "Approved", nil
	})

	body := strings.NewReader(`{
		"update_id": 7,
		"callback_query": {
			"id": "cbq-1",
			"from": {"id": 4242},
			"message": {"message_id": 55, "chat": {"id": 4242}, "text": "Approval required"},
			"data": "approve:apr_1234abcd"
		}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/telegram/webhook", body)
	rec := httptest.NewRecorder()
	adapter.WebhookHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	// The callback is processed asynchronously; wait for the side effects.
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		done := answered && edited != "" && gotDecision != ""
		mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			mu.Lock()
			t.Fatalf("timeout: answered=%v edited=%q decision=%q", answered, edited, gotDecision)
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotUserID != 4242 || gotDecision != "approved" || gotApprovalID != "apr_1234abcd" {
		t.Fatalf("handler got user=%d decision=%q approval=%q", gotUserID, gotDecision, gotApprovalID)
	}
	if !strings.Contains(edited, "✅ approved by you") {
		t.Fatalf("edited text = %q", edited)
	}
}

func TestParseApprovalCallbackData(t *testing.T) {
	cases := []struct {
		data     string
		decision string
		id       string
	}{
		{"approve:apr_x", "approved", "apr_x"},
		{"reject:apr_y", "rejected", "apr_y"},
		{"something-else", "", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		decision, id := parseApprovalCallbackData(c.data)
		if decision != c.decision || id != c.id {
			t.Fatalf("parse(%q) = %q/%q, want %q/%q", c.data, decision, id, c.decision, c.id)
		}
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
