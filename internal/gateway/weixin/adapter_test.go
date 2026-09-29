package weixin

import (
	"context"
	"encoding/json"
	"errors"
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

func TestAdapterCannotStartWithoutDurableInboundStore(t *testing.T) {
	adapter := NewAdapter(RuntimeConfig{Enabled: true, AccountID: "self", Token: "token"}, nil,
		func(context.Context, api.MessageRequest) (api.MessageResponse, int) {
			return api.MessageResponse{}, http.StatusOK
		})
	if err := adapter.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "receipt store") {
		t.Fatalf("start without durable receipt store = %v", err)
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
		AllowFrom:        []string{"wx-user"},
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
	if got.Platform != "weixin" || got.PlatformUserID != "wx-user" || got.Channel != "wx-user" || got.Content != "do work" || !got.Async {
		t.Fatalf("request = %+v", got)
	}
	if got.TenantID != "default" {
		t.Fatalf("tenant = %q", got.TenantID)
	}
	if got.ReplyToRunID != "run_parent" || got.ApprovalID != "apr_parent" || got.ClarifyID != "clarify_parent" {
		t.Fatalf("structured return metadata = %+v", got)
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

func TestOpenDMDoesNotAutoBindUnknownSenderToOwner(t *testing.T) {
	adapter := NewAdapter(RuntimeConfig{
		DMPolicy:      "open",
		OwnerPersonID: "person-owner",
	}, nil, nil)
	if adapter.ownerBindingAllowed("unknown-user", "unknown-user", false) {
		t.Fatal("an open-DM sender must not inherit the owner identity")
	}
	adapter.cfg.AllowFrom = []string{"known-owner"}
	if !adapter.ownerBindingAllowed("known-owner", "known-owner", false) {
		t.Fatal("an explicitly allowlisted owner account should bind")
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
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

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
	}, store, func(ctx context.Context, req api.MessageRequest) (api.MessageResponse, int) {
		if !req.Async {
			t.Fatalf("weixin task messages should be async: %+v", req)
		}
		return api.MessageResponse{Accepted: true}, http.StatusOK
	})

	err = adapter.processMessage(ctx, map[string]interface{}{
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

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ret":0,"errcode":0}`))
	}))
	defer server.Close()
	var calls int
	newAdapter := func() *Adapter {
		return NewAdapter(RuntimeConfig{AccountID: "self", Token: "token", BaseURL: server.URL,
			DMPolicy: "open", GroupPolicy: "disabled", HomeDir: t.TempDir()}, store,
			func(context.Context, api.MessageRequest) (api.MessageResponse, int) {
				calls++
				return api.MessageResponse{}, http.StatusOK
			})
	}
	message := func(id string) map[string]interface{} {
		return map[string]interface{}{"msg": map[string]interface{}{
			"msg_id": id, "from_user_id": "peer", "to_user_id": "self",
			"item_list": []interface{}{map[string]interface{}{"type": itemText,
				"text_item": map[string]interface{}{"text": "do work"}}},
		}}
	}
	first := newAdapter()
	if err := first.processMessage(ctx, message("wx-msg-1")); err != nil {
		t.Fatal(err)
	}
	if err := first.processMessage(ctx, message("wx-msg-1")); err != nil {
		t.Fatal(err)
	}
	// A fresh adapter simulates a daemon restart and replay of the old cursor.
	second := newAdapter()
	if err := second.processMessage(ctx, message("wx-msg-1")); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("accepted input dispatched %d times, want once", calls)
	}
	if err := second.processMessage(ctx, message("wx-msg-2")); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("new input dispatched %d times total, want two", calls)
	}
}

func TestWeixinDoesNotReplayUncertainInbound(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ret":0,"errcode":0}`))
	}))
	defer server.Close()
	raw := map[string]interface{}{"msg": map[string]interface{}{
		"msg_id": "uncertain-1", "from_user_id": "peer", "to_user_id": "self",
		"item_list": []interface{}{map[string]interface{}{"type": itemText,
			"text_item": map[string]interface{}{"text": "do work"}}},
	}}
	payload, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "peer", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginInbound(ctx, "weixin", "uncertain-1", payload,
		control.InboundOwner{TenantID: identity.TenantID, PersonID: identity.PersonID}); err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimInbound(ctx, "weixin", "uncertain-1"); err != nil || !claimed {
		t.Fatalf("claim = %t, %v", claimed, err)
	}
	var calls int
	adapter := NewAdapter(RuntimeConfig{AccountID: "self", Token: "token", BaseURL: server.URL,
		DMPolicy: "open", GroupPolicy: "disabled", HomeDir: t.TempDir()}, store,
		func(context.Context, api.MessageRequest) (api.MessageResponse, int) {
			calls++
			return api.MessageResponse{}, http.StatusOK
		})
	if err := adapter.processMessage(ctx, raw); !errors.Is(err, errInboundUncertain) || calls != 0 {
		t.Fatalf("replayed uncertain input: handler calls=%d err=%v", calls, err)
	}
}
