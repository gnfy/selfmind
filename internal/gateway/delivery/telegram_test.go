package delivery

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTelegramSenderAttachesApprovalButtons(t *testing.T) {
	var payload map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bottok/sendMessage" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	sender := &TelegramSender{Token: "tok", BaseURL: server.URL, Client: server.Client()}
	err := sender.Send(context.Background(), Message{
		Channel:    "12345",
		Content:    "Approval required",
		Kind:       KindApproval,
		ApprovalID: "apr_abc123",
	})
	if err != nil {
		t.Fatal(err)
	}

	markup, ok := payload["reply_markup"].(map[string]interface{})
	if !ok {
		t.Fatalf("reply_markup missing: %+v", payload)
	}
	rows, ok := markup["inline_keyboard"].([]interface{})
	if !ok || len(rows) != 2 {
		t.Fatalf("inline_keyboard = %+v", markup["inline_keyboard"])
	}
	first := rows[0].([]interface{})[0].(map[string]interface{})
	second := rows[1].([]interface{})[0].(map[string]interface{})
	if first["callback_data"] != "approve:apr_abc123" || second["callback_data"] != "reject:apr_abc123" {
		t.Fatalf("callback data = %v / %v", first["callback_data"], second["callback_data"])
	}
}

func TestTelegramSenderPlainMessageHasNoButtons(t *testing.T) {
	var payload map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&payload)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	sender := &TelegramSender{Token: "tok", BaseURL: server.URL, Client: server.Client()}
	if err := sender.Send(context.Background(), Message{Channel: "12345", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["reply_markup"]; ok {
		t.Fatalf("unexpected reply_markup: %+v", payload)
	}
}
