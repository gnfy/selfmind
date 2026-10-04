package delivery

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"selfmind/internal/control"
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

func TestTelegramNativeReplyReceiptReachesDurableOutbox(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":87}}`))
	}))
	defer server.Close()
	router := NewRouter(nil)
	router.Register("telegram", &TelegramSender{Token: "tok", BaseURL: server.URL, Client: server.Client()})
	svc := NewService(store, router, Options{})
	if err := svc.EnqueueAndTry(ctx, Message{
		TenantID: "default", PersonID: "owner", Platform: "telegram", PlatformUserID: "user-1",
		Channel: "chat-1", RunID: "run-1", ClarifyID: "clarify-1", Kind: KindClarify, Content: "Which region?",
	}); err != nil {
		t.Fatal(err)
	}
	edge, err := store.NativeIMReplyTarget(ctx, "default", "owner", "telegram", "chat-1", "87")
	if err != nil || edge.RunID != "run-1" || edge.ClarifyID != "clarify-1" {
		t.Fatalf("durable Telegram reply edge: %+v, %v", edge, err)
	}
}

func TestNativeReceiptCollisionDoesNotResendAcceptedMessage(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sends := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends++
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":87}}`))
	}))
	defer server.Close()
	router := NewRouter(nil)
	router.Register("telegram", &TelegramSender{Token: "tok", BaseURL: server.URL, Client: server.Client()})
	svc := NewService(store, router, Options{})
	for i, person := range []string{"owner-a", "owner-b"} {
		err := svc.EnqueueAndTry(ctx, Message{TenantID: "default", PersonID: person, Platform: "telegram",
			Channel: "chat-1", RunID: person, Content: "work " + person})
		if (i == 0 && err != nil) || (i == 1 && err == nil) {
			t.Fatalf("send %d receipt error = %v", i, err)
		}
	}
	if sends != 2 {
		t.Fatalf("initial accepted sends = %d", sends)
	}
	svc.flushDue(ctx)
	if sends != 2 {
		t.Fatalf("receipt collision caused blind resend: %d", sends)
	}
	rows, err := store.ListDueDeliveries(ctx, 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("accepted messages remain retryable: %+v, %v", rows, err)
	}
}
