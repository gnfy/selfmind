package weixin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/gateway/router"
)

func TestAdapterWaitsForCredentialRefresh(t *testing.T) {
	home := t.TempDir()
	adapter := NewAdapter(RuntimeConfig{
		AccountID: "wx-account",
		Token:     "expired-token",
		BaseURL:   "https://old.example",
		HomeDir:   home,
	}, nil, nil)
	adapter.credentialRefreshInterval = time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	expiredAt := time.Now().UTC()
	go func() {
		time.Sleep(10 * time.Millisecond)
		_ = SaveCredentials(home, &Credentials{
			AccountID: "wx-account",
			Token:     "fresh-token",
			BaseURL:   "https://fresh.example",
			SavedAt:   time.Now().UTC().Format(time.RFC3339Nano),
		})
	}()

	if !adapter.waitForCredentialRefresh(ctx, "expired-token", expiredAt) {
		t.Fatal("credential refresh was not detected")
	}
	client := adapter.clientSnapshot()
	if client.cfg.Token != "fresh-token" {
		t.Fatalf("token = %q, want fresh-token", client.cfg.Token)
	}
	if client.cfg.BaseURL != "https://fresh.example" {
		t.Fatalf("base URL = %q", client.cfg.BaseURL)
	}
}

func TestCredentialRefreshesSessionWithSameTokenAfterRelogin(t *testing.T) {
	expiredAt := time.Now().UTC().Add(-time.Second)
	cred := &Credentials{
		Token:   "same-token",
		SavedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if !credentialRefreshesSession(cred, "same-token", expiredAt) {
		t.Fatal("a newer credential file must refresh the session even when the token is unchanged")
	}
	cred.SavedAt = expiredAt.Add(-time.Second).Format(time.RFC3339Nano)
	if credentialRefreshesSession(cred, "same-token", expiredAt) {
		t.Fatal("an older credential file must not refresh the session")
	}
}

func TestAdapterProactiveSendUsesConcreteRecipient(t *testing.T) {
	var target string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		msg, _ := payload["msg"].(map[string]interface{})
		target = stringFromMap(msg, "to_user_id")
		_, _ = w.Write([]byte(`{"ret":0,"errcode":0}`))
	}))
	defer server.Close()

	adapter := NewAdapter(RuntimeConfig{
		AccountID: "wx-account", Token: "token", BaseURL: server.URL,
		HomeDir: t.TempDir(), SendChunkRetries: 0,
	}, nil, nil)
	_, err := adapter.SendWithReceipt(context.Background(), delivery.Message{
		Platform: "weixin", PlatformUserID: "real-peer@im.wechat", Channel: "weixin", Content: "reminder",
	})
	if err != nil {
		t.Fatal(err)
	}
	if target != "real-peer@im.wechat" {
		t.Fatalf("to_user_id = %q; generic channel must fall back to recipient", target)
	}
}

func TestAdapterProcessesMessageThroughGatewayHandler(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Owner")
	if err != nil {
		t.Fatal(err)
	}

	var sentMessages int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + epGetConfig:
			_, _ = w.Write([]byte(`{"ret":0,"errcode":0,"typing_ticket":"ticket-1"}`))
		case "/" + epSendTyping:
			_, _ = w.Write([]byte(`{"ret":0,"errcode":0}`))
		case "/" + epSendMessage:
			sentMessages++
			_, _ = w.Write([]byte(`{"ret":0,"errcode":0}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	var got api.MessageRequest
	adapter := NewAdapter(RuntimeConfig{
		Enabled:          true,
		AccountID:        "wx-account",
		Token:            "token",
		BaseURL:          server.URL,
		CDNBaseURL:       server.URL,
		DMPolicy:         "open",
		GroupPolicy:      "disabled",
		OwnerPersonID:    owner.PersonID,
		DefaultTenantID:  "default",
		HomeDir:          t.TempDir(),
		SendChunkRetries: 1,
	}, store, func(ctx context.Context, req api.MessageRequest) (api.MessageResponse, int) {
		got = req
		return api.MessageResponse{Content: "ack"}, http.StatusOK
	})

	err = adapter.processMessage(ctx, map[string]interface{}{
		"msg": map[string]interface{}{
			"msg_id":        "m1",
			"from_user_id":  "wx-user",
			"to_user_id":    "wx-account",
			"context_token": "ctx-token",
			"item_list": []interface{}{
				map[string]interface{}{
					"type": itemText,
					"text_item": map[string]interface{}{
						"text": "do work",
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Platform != "weixin" || got.PlatformUserID != "wx-user" || got.Channel != "wx-user" || got.Content != "do work" || !got.Async {
		t.Fatalf("request = %+v", got)
	}
	if got.TenantID != "default" {
		t.Fatalf("tenant = %q", got.TenantID)
	}
	if sentMessages != 1 {
		t.Fatalf("sent messages = %d", sentMessages)
	}
	if token := adapter.client.tokens.Get("wx-account", "wx-user"); token != "ctx-token" {
		t.Fatalf("context token = %q", token)
	}
	bound, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx-user", "")
	if err != nil {
		t.Fatal(err)
	}
	if bound.PersonID != owner.PersonID {
		payload, _ := json.MarshalIndent(bound, "", "  ")
		t.Fatalf("bound account = %s, owner=%s", payload, owner.PersonID)
	}
}

func TestAdapterGroupPolicyDefaultsToDisabled(t *testing.T) {
	adapter := NewAdapter(RuntimeConfig{DMPolicy: "open", GroupPolicy: "disabled"}, nil, nil)
	if !adapter.allowed("u1", "u1", false) {
		t.Fatal("direct messages should be allowed by open policy")
	}
	if adapter.allowed("u1", "room@chatroom", true) {
		t.Fatal("groups should be disabled by default")
	}
}

func TestAdapterSendsWorkingNoticeForAcceptedAsyncRun(t *testing.T) {
	ctx := context.Background()
	var sentTexts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + epGetConfig:
			_, _ = w.Write([]byte(`{"ret":0,"errcode":0,"typing_ticket":"ticket-1"}`))
		case "/" + epSendTyping:
			_, _ = w.Write([]byte(`{"ret":0,"errcode":0}`))
		case "/" + epSendMessage:
			var payload map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode send payload: %v", err)
			}
			sentTexts = append(sentTexts, weixinTextFromSendPayload(payload))
			_, _ = w.Write([]byte(`{"ret":0,"errcode":0}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	adapter := NewAdapter(RuntimeConfig{
		Enabled:          true,
		AccountID:        "wx-account",
		Token:            "token",
		BaseURL:          server.URL,
		CDNBaseURL:       server.URL,
		DMPolicy:         "open",
		GroupPolicy:      "disabled",
		HomeDir:          t.TempDir(),
		SendChunkRetries: 1,
	}, nil, func(ctx context.Context, req api.MessageRequest) (api.MessageResponse, int) {
		if !req.Async {
			t.Fatalf("weixin task messages should be async: %+v", req)
		}
		return api.MessageResponse{Accepted: true}, http.StatusOK
	})

	err := adapter.processMessage(ctx, map[string]interface{}{
		"msg": map[string]interface{}{
			"msg_id":       "m-accepted",
			"from_user_id": "wx-user",
			"to_user_id":   "wx-account",
			"item_list": []interface{}{
				map[string]interface{}{
					"type": itemText,
					"text_item": map[string]interface{}{
						"text": "帮我跑测试",
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sentTexts) != 1 {
		t.Fatalf("sent texts = %+v", sentTexts)
	}
	if sentTexts[0] != router.WorkingNotice("weixin") {
		t.Fatalf("sent text = %q", sentTexts[0])
	}
}

func weixinTextFromSendPayload(payload map[string]interface{}) string {
	msg, _ := payload["msg"].(map[string]interface{})
	items, _ := msg["item_list"].([]interface{})
	if len(items) == 0 {
		return ""
	}
	item, _ := items[0].(map[string]interface{})
	textItem, _ := item["text_item"].(map[string]interface{})
	text, _ := textItem["text"].(string)
	return text
}

func TestDuplicateDetectionSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	first := NewAdapter(RuntimeConfig{DMPolicy: "open", GroupPolicy: "disabled"}, store, nil)
	if first.isDuplicate(ctx, "wx-msg-1") {
		t.Fatal("first sighting must not be a duplicate")
	}
	if !first.isDuplicate(ctx, "wx-msg-1") {
		t.Fatal("in-memory repeat must be a duplicate")
	}

	// A fresh adapter simulates a daemon restart: the in-memory map is empty
	// but the sync buffer replays recent messages — the durable store is what
	// must remember them.
	second := NewAdapter(RuntimeConfig{DMPolicy: "open", GroupPolicy: "disabled"}, store, nil)
	if !second.isDuplicate(ctx, "wx-msg-1") {
		t.Fatal("duplicate detection must survive a restart via the durable store")
	}
	if second.isDuplicate(ctx, "wx-msg-2") {
		t.Fatal("an unseen id must not be a duplicate")
	}
}
