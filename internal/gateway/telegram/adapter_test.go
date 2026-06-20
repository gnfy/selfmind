package telegram

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
