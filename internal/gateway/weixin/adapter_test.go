package weixin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	}, nil)
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

func TestAdapterCannotStartWithoutGatewayInboundHandler(t *testing.T) {
	adapter := NewAdapter(RuntimeConfig{Enabled: true, AccountID: "self", Token: "token"}, nil)
	if err := adapter.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "handler") {
		t.Fatalf("start without gateway inbound handler = %v", err)
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
	}, nil)
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

	var got api.DurableInbound
	adapter := NewAdapter(RuntimeConfig{
		Enabled:          true,
		AccountID:        "wx-account",
		Token:            "token",
		BaseURL:          server.URL,
		CDNBaseURL:       server.URL,
		DMPolicy:         "open",
		AllowFrom:        []string{"wx-user"},
		GroupPolicy:      "disabled",
		OwnerPersonID:    owner.PersonID,
		DefaultTenantID:  "default",
		HomeDir:          t.TempDir(),
		SendChunkRetries: 1,
	}, func(ctx context.Context, inbound api.DurableInbound) (api.MessageResponse, int, error) {
		got = inbound
		return api.MessageResponse{Content: "ack"}, http.StatusOK, nil
	})

	err = adapter.processMessage(ctx, map[string]interface{}{
		"msg": map[string]interface{}{
			"msg_id":          "m1",
			"from_user_id":    "wx-user",
			"to_user_id":      "wx-account",
			"context_token":   "ctx-token",
			"reply_to_run_id": "run_parent",
			"approval_id":     "apr_parent",
			"clarify_id":      "clarify_parent",
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
	if got.Request.Platform != "weixin" || got.Request.PlatformUserID != "wx-user" || got.Request.Channel != "wx-user" || got.Request.Content != "do work" || !got.Request.Async {
		t.Fatalf("request = %+v", got)
	}
	if got.Request.TenantID != "default" {
		t.Fatalf("tenant = %q", got.Request.TenantID)
	}
	if got.Request.ReplyToRunID != "run_parent" || got.Request.ApprovalID != "apr_parent" || got.Request.ClarifyID != "clarify_parent" {
		t.Fatalf("structured return metadata = %+v", got)
	}
	if got.MessageID != "m1" || got.OwnerPersonID != owner.PersonID || len(got.RawPayload) == 0 {
		t.Fatalf("durable gateway ingress metadata = %+v", got)
	}
	if sentMessages != 1 {
		t.Fatalf("sent messages = %d", sentMessages)
	}
	if token := adapter.client.tokens.Get("wx-account", "wx-user"); token != "ctx-token" {
		t.Fatalf("context token = %q", token)
	}
}

func TestOpenDMDoesNotAutoBindUnknownSenderToOwner(t *testing.T) {
	adapter := NewAdapter(RuntimeConfig{
		DMPolicy:      "open",
		OwnerPersonID: "person-owner",
	}, nil)
	if adapter.ownerBindingAllowed("unknown-user", "unknown-user", false) {
		t.Fatal("an open-DM sender must not inherit the owner identity")
	}
	adapter.cfg.AllowFrom = []string{"known-owner"}
	if !adapter.ownerBindingAllowed("known-owner", "known-owner", false) {
		t.Fatal("an explicitly allowlisted owner account should bind")
	}
}

func TestAdapterGroupPolicyDefaultsToDisabled(t *testing.T) {
	adapter := NewAdapter(RuntimeConfig{DMPolicy: "open", GroupPolicy: "disabled"}, nil)
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
	}, func(ctx context.Context, inbound api.DurableInbound) (api.MessageResponse, int, error) {
		if !inbound.Request.Async {
			t.Fatalf("weixin task messages should be async: %+v", inbound.Request)
		}
		return api.MessageResponse{Accepted: true}, http.StatusOK, nil
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

func TestWeixinWorkWithoutMessageIDDoesNotDispatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ret":0,"errcode":0}`))
	}))
	defer server.Close()
	var calls int
	adapter := NewAdapter(RuntimeConfig{AccountID: "self", Token: "token", BaseURL: server.URL,
		DMPolicy: "open", GroupPolicy: "disabled", HomeDir: t.TempDir()},
		func(context.Context, api.DurableInbound) (api.MessageResponse, int, error) {
			calls++
			return api.MessageResponse{}, http.StatusOK, nil
		})
	err := adapter.processMessage(context.Background(), map[string]interface{}{"msg": map[string]interface{}{
		"from_user_id": "peer", "to_user_id": "self", "item_list": []interface{}{
			map[string]interface{}{"type": itemText, "text_item": map[string]interface{}{"text": "do work"}},
		},
	}})
	if err == nil || !strings.Contains(err.Error(), "stable platform message id") || calls != 0 {
		t.Fatalf("idless message dispatched: calls=%d err=%v", calls, err)
	}
}
