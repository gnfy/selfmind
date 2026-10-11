package weixin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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
	if got.Essential == nil {
		t.Fatal("the adapter did not say which fields identify its messages")
	}
	if essential, err := got.Essential(got.RawPayload); err != nil || !strings.Contains(string(essential), `"text":"do work"`) {
		t.Fatalf("essential fields of the delivered message = %s, %v", essential, err)
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

// fakeILink serves one inbound message until the adapter advances past it and
// records what the adapter asked for and sent.
type fakeILink struct {
	mu      sync.Mutex
	cursors []string
	sent    []string
}

func newFakeILink(t *testing.T) (*fakeILink, *httptest.Server) {
	t.Helper()
	return newFakeILinkServing(t, "cursor-after-m1", `{"msg_id":"m1",`+
		`"from_user_id":"wx-user","to_user_id":"wx-account","item_list":[{"type":1,"text_item":{"text":"start the release"}}]}`)
}

// newFakeILinkServing serves one batch of messages until the adapter advances
// past it to the next cursor.
func newFakeILinkServing(t *testing.T, next string, msgs ...string) (*fakeILink, *httptest.Server) {
	t.Helper()
	fake := &fakeILink{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		fake.mu.Lock()
		defer fake.mu.Unlock()
		switch r.URL.Path {
		case "/" + epGetUpdates:
			cursor := stringFromMap(payload, "get_updates_buf")
			fake.cursors = append(fake.cursors, cursor)
			if cursor != "" {
				_, _ = w.Write([]byte(`{"ret":0,"errcode":0,"msgs":[],"get_updates_buf":"` + cursor + `"}`))
				return
			}
			_, _ = w.Write([]byte(`{"ret":0,"errcode":0,"get_updates_buf":"` + next + `","msgs":[` + strings.Join(msgs, ",") + `]}`))
		case "/" + epSendMessage:
			fake.sent = append(fake.sent, weixinTextFromSendPayload(payload))
			_, _ = w.Write([]byte(`{"ret":0,"errcode":0}`))
		default:
			_, _ = w.Write([]byte(`{"ret":0,"errcode":0}`))
		}
	}))
	t.Cleanup(server.Close)
	return fake, server
}

func (f *fakeILink) snapshot() (cursors, sent []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cursors...), append([]string(nil), f.sent...)
}

func runPollLoopUntil(t *testing.T, adapter *Adapter, done func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	go adapter.pollLoop(ctx)
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			cancel()
			<-adapter.done
			t.Fatal("poll loop did not reach the expected state")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-adapter.done
}

func countContaining(values []string, needle string) int {
	n := 0
	for _, value := range values {
		if strings.Contains(value, needle) {
			n++
		}
	}
	return n
}

// A message the gateway refused before admission stays redeliverable: the
// sender hears why once, the sync cursor does not move past it, and after a
// restart the same message is dispatched once from the retained cursor.
func TestAdapterKeepsRefusedMessageRedeliverable(t *testing.T) {
	fake, server := newFakeILink(t)
	home := t.TempDir()
	cfg := RuntimeConfig{Enabled: true, AccountID: "wx-account", Token: "token", BaseURL: server.URL, CDNBaseURL: server.URL,
		DMPolicy: "open", GroupPolicy: "disabled", HomeDir: home, SendChunkRetries: 0}
	notice := "This message was not started because SelfMind is shutting down. Nothing ran; it will be retried automatically."
	var refusals atomic.Int32
	refusing := NewAdapter(cfg, func(_ context.Context, inbound api.DurableInbound) (api.MessageResponse, int, error) {
		refusals.Add(1)
		return api.MessageResponse{Error: notice}, http.StatusServiceUnavailable,
			fmt.Errorf("weixin message %s: %w", inbound.MessageID, api.ErrInboundNotDispatched)
	})
	refusing.retryDelay = time.Millisecond
	runPollLoopUntil(t, refusing, func() bool { return refusals.Load() >= 3 })
	cursors, sent := fake.snapshot()
	if countContaining(sent, "not started") != 1 {
		t.Fatalf("refusal notices = %q; want exactly one", sent)
	}
	for _, cursor := range cursors {
		if cursor != "" {
			t.Fatalf("cursor advanced past a refused message: %q", cursors)
		}
	}
	if _, err := os.Stat(SyncBufFilePath(home, "wx-account")); !os.IsNotExist(err) {
		t.Fatalf("sync cursor was saved past a refused message: %v", err)
	}

	var dispatched atomic.Int32
	restarted := NewAdapter(cfg, func(_ context.Context, inbound api.DurableInbound) (api.MessageResponse, int, error) {
		if inbound.MessageID != "m1" {
			t.Errorf("unexpected message %q", inbound.MessageID)
		}
		dispatched.Add(1)
		return api.MessageResponse{Content: "release started"}, http.StatusOK, nil
	})
	restarted.retryDelay = time.Millisecond
	runPollLoopUntil(t, restarted, func() bool {
		data, err := os.ReadFile(SyncBufFilePath(home, "wx-account"))
		return err == nil && strings.TrimSpace(string(data)) == "cursor-after-m1"
	})
	if dispatched.Load() != 1 {
		t.Fatalf("retained message dispatched %d times after restart, want 1", dispatched.Load())
	}
	_, sent = fake.snapshot()
	if countContaining(sent, "release started") != 1 || countContaining(sent, "not started") != 1 {
		t.Fatalf("chat messages after redelivery = %q", sent)
	}
}

// A message the gateway rejected for good can never be admitted by retrying:
// the sender hears it once and the cursor moves past it, so later messages
// are not blocked behind it.
func TestAdapterSkipsRejectedMessageAfterOneNotice(t *testing.T) {
	fake, server := newFakeILink(t)
	home := t.TempDir()
	var calls atomic.Int32
	adapter := NewAdapter(RuntimeConfig{Enabled: true, AccountID: "wx-account", Token: "token", BaseURL: server.URL,
		CDNBaseURL: server.URL, DMPolicy: "open", GroupPolicy: "disabled", HomeDir: home, SendChunkRetries: 0},
		func(_ context.Context, inbound api.DurableInbound) (api.MessageResponse, int, error) {
			calls.Add(1)
			return api.MessageResponse{Error: "This message was not started: it does not match an earlier delivery with the same ID."},
				http.StatusConflict, fmt.Errorf("weixin message %s: %w", inbound.MessageID, api.ErrInboundRejected)
		})
	adapter.retryDelay = time.Millisecond
	runPollLoopUntil(t, adapter, func() bool {
		cursors, _ := fake.snapshot()
		return countContaining(cursors, "cursor-after-m1") > 0
	})
	_, sent := fake.snapshot()
	if calls.Load() != 1 || countContaining(sent, "not started") != 1 {
		t.Fatalf("rejected message: dispatches=%d notices=%q; want one of each", calls.Load(), sent)
	}
}

// "Not started, will be retried" and "rejected, send it again" say different
// things about the same message. A refusal notice must not swallow the later
// rejection notice for that message.
func TestAdapterNoticesRefusalAndRejectionSeparately(t *testing.T) {
	fake, server := newFakeILink(t)
	var calls atomic.Int32
	adapter := NewAdapter(RuntimeConfig{Enabled: true, AccountID: "wx-account", Token: "token", BaseURL: server.URL,
		CDNBaseURL: server.URL, DMPolicy: "open", GroupPolicy: "disabled", HomeDir: t.TempDir(), SendChunkRetries: 0},
		func(_ context.Context, inbound api.DurableInbound) (api.MessageResponse, int, error) {
			if calls.Add(1) == 1 {
				return api.MessageResponse{Error: "This message was not started because SelfMind is shutting down. Nothing ran; it will be retried automatically."},
					http.StatusServiceUnavailable, fmt.Errorf("weixin message %s: %w", inbound.MessageID, api.ErrInboundNotDispatched)
			}
			return api.MessageResponse{Error: "This message was not started: it does not match an earlier delivery with the same ID, so it will not be retried."},
				http.StatusConflict, fmt.Errorf("weixin message %s: %w", inbound.MessageID, api.ErrInboundRejected)
		})
	adapter.retryDelay = time.Millisecond
	runPollLoopUntil(t, adapter, func() bool {
		cursors, _ := fake.snapshot()
		return countContaining(cursors, "cursor-after-m1") > 0
	})
	_, sent := fake.snapshot()
	if countContaining(sent, "retried automatically") != 1 || countContaining(sent, "will not be retried") != 1 {
		t.Fatalf("notices = %q; want one refusal and one rejection", sent)
	}
}

// A redelivered getupdates entry may carry a new context token, server
// timestamp, or media download parameters. Those do not make it a different
// message; a different id, sender, text, routing edge, or media object does.
// Media identity is the object's content key, so two images differ even
// though neither has a file name.
func TestEssentialMessageIgnoresTransportFields(t *testing.T) {
	adapter := NewAdapter(RuntimeConfig{AccountID: "wx-account"}, nil)
	type parts struct{ msg, file, fileMedia, image, imageMedia map[string]interface{} }
	entry := func(mutate func(p parts)) []byte {
		t.Helper()
		p := parts{
			fileMedia:  map[string]interface{}{"encrypt_query_param": "q-1", "full_url": "https://cdn.example/d?q=1", "aes_key": "file-key-1"},
			imageMedia: map[string]interface{}{"encrypt_query_param": "q-3", "full_url": "https://cdn.example/d?q=3", "aes_key": "image-key-1"},
		}
		p.file = map[string]interface{}{"file_name": "plan.pdf", "media": p.fileMedia}
		p.image = map[string]interface{}{"media": p.imageMedia}
		p.msg = map[string]interface{}{
			"msg_id": "m1", "from_user_id": "wx-user", "to_user_id": "wx-account",
			"context_token": "token-1", "create_time_ms": 1000, "seq": 7,
			"item_list": []interface{}{
				map[string]interface{}{"type": itemText, "text_item": map[string]interface{}{"text": "ship it"}},
				map[string]interface{}{"type": itemFile, "file_item": p.file},
				map[string]interface{}{"type": itemImage, "image_item": p.image},
			},
		}
		if mutate != nil {
			mutate(p)
		}
		raw, err := json.Marshal(map[string]interface{}{"msg": p.msg})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	essential := func(raw []byte) string {
		t.Helper()
		out, err := adapter.essentialMessage(raw)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	base := essential(entry(nil))
	for name, mutate := range map[string]func(parts){
		"context token": func(p parts) { p.msg["context_token"] = "token-2" },
		"server time":   func(p parts) { p.msg["create_time_ms"], p.msg["seq"] = 2000, 8 },
		"media download": func(p parts) {
			p.fileMedia["encrypt_query_param"], p.fileMedia["full_url"] = "q-2", "https://cdn.example/d?q=2"
			p.imageMedia["encrypt_query_param"], p.imageMedia["full_url"] = "q-4", "https://cdn.example/d?q=4"
		},
	} {
		if got := essential(entry(mutate)); got != base {
			t.Fatalf("%s change made a different message: %s vs %s", name, got, base)
		}
	}
	for name, mutate := range map[string]func(parts){
		"message id": func(p parts) { p.msg["msg_id"] = "m2" },
		"sender":     func(p parts) { p.msg["from_user_id"] = "wx-other" },
		"text": func(p parts) {
			p.msg["item_list"].([]interface{})[0] = map[string]interface{}{"type": itemText, "text_item": map[string]interface{}{"text": "stop it"}}
		},
		"file name":    func(p parts) { p.file["file_name"] = "other.pdf" },
		"file content": func(p parts) { p.fileMedia["aes_key"] = "file-key-2" },
		"image":        func(p parts) { p.imageMedia["aes_key"] = "image-key-2" },
		"image key":    func(p parts) { p.image["aeskey"] = "00ff" },
		"reply edge":   func(p parts) { p.msg["reply_to_run_id"] = "run-other" },
	} {
		if got := essential(entry(mutate)); got == base {
			t.Fatalf("%s change still matched the original message", name)
		}
	}
}

// An uncertain receipt is different: the gateway may already have acted, so
// the adapter must not replay it and the cursor moves on. Its response may
// carry an internal error, which never reaches the chat.
func TestAdapterSkipsUncertainMessageAndAdvancesCursor(t *testing.T) {
	fake, server := newFakeILink(t)
	home := t.TempDir()
	var calls atomic.Int32
	adapter := NewAdapter(RuntimeConfig{Enabled: true, AccountID: "wx-account", Token: "token", BaseURL: server.URL,
		CDNBaseURL: server.URL, DMPolicy: "open", GroupPolicy: "disabled", HomeDir: home, SendChunkRetries: 0},
		func(_ context.Context, inbound api.DurableInbound) (api.MessageResponse, int, error) {
			calls.Add(1)
			return api.MessageResponse{Error: "constraint failed: injected store error (1811)"}, http.StatusInternalServerError,
				fmt.Errorf("weixin message %s: %w", inbound.MessageID, api.ErrInboundUncertain)
		})
	adapter.retryDelay = time.Millisecond
	runPollLoopUntil(t, adapter, func() bool {
		cursors, _ := fake.snapshot()
		return countContaining(cursors, "cursor-after-m1") > 0
	})
	if calls.Load() != 1 {
		t.Fatalf("uncertain message dispatched %d times, want 1", calls.Load())
	}
	if _, sent := fake.snapshot(); countContaining(sent, "constraint failed") != 0 {
		t.Fatalf("an internal error reached the chat: %q", sent)
	}
}

// An answer that claimed its choice but whose request could not be started is
// held like any uncertain receipt, yet its sender must hear it: the gateway's
// held notice is sent once, even when a replayed batch delivers the message
// again, and separately from the refusal notice of another message.
func TestAdapterNoticesHeldAnswerOnce(t *testing.T) {
	fake, server := newFakeILinkServing(t, "cursor-after-batch",
		`{"msg_id":"m1","from_user_id":"wx-user","to_user_id":"wx-account","item_list":[{"type":1,"text_item":{"text":"2"}}]}`,
		`{"msg_id":"m2","from_user_id":"wx-user","to_user_id":"wx-account","item_list":[{"type":1,"text_item":{"text":"and the docs"}}]}`)
	held := "Your answer was recorded, but its request was not started because SelfMind could not save the message. " +
		"Check /diag inbound, then send the request again if it is still needed."
	var answers, refusals atomic.Int32
	adapter := NewAdapter(RuntimeConfig{Enabled: true, AccountID: "wx-account", Token: "token", BaseURL: server.URL,
		CDNBaseURL: server.URL, DMPolicy: "open", GroupPolicy: "disabled", HomeDir: t.TempDir(), SendChunkRetries: 0},
		func(_ context.Context, inbound api.DurableInbound) (api.MessageResponse, int, error) {
			if inbound.MessageID == "m1" {
				answers.Add(1)
				return api.MessageResponse{Error: held}, http.StatusServiceUnavailable,
					fmt.Errorf("weixin message %s: %w", inbound.MessageID, api.ErrInboundHeld)
			}
			// The second message is refused twice, so the batch is replayed.
			if refusals.Add(1) <= 2 {
				return api.MessageResponse{Error: "This message was not started because SelfMind is shutting down. Nothing ran; it will be retried automatically."},
					http.StatusServiceUnavailable, fmt.Errorf("weixin message %s: %w", inbound.MessageID, api.ErrInboundNotDispatched)
			}
			return api.MessageResponse{Content: "docs queued"}, http.StatusOK, nil
		})
	adapter.retryDelay = time.Millisecond
	runPollLoopUntil(t, adapter, func() bool {
		cursors, _ := fake.snapshot()
		return countContaining(cursors, "cursor-after-batch") > 0
	})
	_, sent := fake.snapshot()
	if answers.Load() < 2 {
		t.Fatalf("the held answer was delivered %d times; the replay did not happen", answers.Load())
	}
	if countContaining(sent, "Your answer was recorded") != 1 {
		t.Fatalf("held notices = %q; want exactly one", sent)
	}
	if countContaining(sent, "retried automatically") != 1 || countContaining(sent, "docs queued") != 1 {
		t.Fatalf("chat messages = %q; want one refusal notice and the reply", sent)
	}
}

func TestAdapterDoesNotResendDurablyEnqueuedChoice(t *testing.T) {
	fake, server := newFakeILinkServing(t, "choice-accepted",
		`{"msg_id":"choice-message","from_user_id":"wx-user","to_user_id":"wx-account","item_list":[{"type":1,"text_item":{"text":"inspect the other service"}}]}`)
	adapter := NewAdapter(RuntimeConfig{Enabled: true, AccountID: "wx-account", Token: "token", BaseURL: server.URL,
		CDNBaseURL: server.URL, DMPolicy: "open", GroupPolicy: "disabled", HomeDir: t.TempDir(), SendChunkRetries: 0},
		func(context.Context, api.DurableInbound) (api.MessageResponse, int, error) {
			return api.MessageResponse{Content: "Which work is this for?", Choice: &api.TurnChoice{ID: "choice_saved"}, DeliveryEnqueued: true}, http.StatusOK, nil
		})
	runPollLoopUntil(t, adapter, func() bool { cursors, _ := fake.snapshot(); return countContaining(cursors, "choice-accepted") > 0 })
	_, sent := fake.snapshot()
	if len(sent) != 0 {
		t.Fatalf("adapter duplicated the outbox-owned notice: %q", sent)
	}
}
