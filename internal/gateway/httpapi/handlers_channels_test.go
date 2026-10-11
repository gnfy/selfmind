package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
)

func feishuSig(ts, nonce, key string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(ts + nonce + key))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

func TestVerifyFeishuSignature(t *testing.T) {
	t.Setenv("SELF_FEISHU_ENCRYPT_KEY", "test-encrypt-key")
	t.Setenv("SELF_FEISHU_VERIFICATION_TOKEN", "")

	body := []byte(`{"type":"event_callback"}`)
	ts, nonce := "1700000000", "abc123"

	t.Run("valid signature passes", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/v1/im/feishu", nil)
		req.Header.Set("X-Lark-Request-Timestamp", ts)
		req.Header.Set("X-Lark-Request-Nonce", nonce)
		req.Header.Set("X-Lark-Signature", feishuSig(ts, nonce, "test-encrypt-key", body))
		if err := verifyFeishuSignature(req, body, nil); err != nil {
			t.Fatalf("expected valid signature to pass, got %v", err)
		}
	})

	t.Run("wrong signature rejected", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/v1/im/feishu", nil)
		req.Header.Set("X-Lark-Request-Timestamp", ts)
		req.Header.Set("X-Lark-Request-Nonce", nonce)
		req.Header.Set("X-Lark-Signature", "deadbeef")
		if err := verifyFeishuSignature(req, body, nil); err == nil {
			t.Fatal("expected wrong signature to be rejected")
		}
	})

	t.Run("missing signature rejected", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/v1/im/feishu", nil)
		if err := verifyFeishuSignature(req, body, nil); err == nil {
			t.Fatal("expected missing signature to be rejected")
		}
	})
}

func TestVerifyIMSignatureNoSecretAllows(t *testing.T) {
	t.Setenv("SELF_FEISHU_ENCRYPT_KEY", "")
	t.Setenv("SELF_FEISHU_VERIFICATION_TOKEN", "")
	req := httptest.NewRequest("POST", "/v1/im/feishu", nil)
	if err := verifyIMSignature("feishu", req, []byte(`{}`), nil); err != nil {
		t.Fatalf("unconfigured platform should allow, got %v", err)
	}
	if err := verifyIMSignature("webhook", req, []byte(`{}`), nil); err != nil {
		t.Fatalf("generic webhook should allow, got %v", err)
	}
}

func TestIMMessageIDExtraction(t *testing.T) {
	cases := []struct {
		name     string
		platform string
		payload  string
		want     string
	}{
		{"generic message_id", "webhook", `{"message_id":"m-1"}`, "m-1"},
		{"feishu v2 header event_id", "feishu", `{"header":{"event_id":"ev-1"}}`, "ev-1"},
		{"feishu event message id", "feishu", `{"event":{"message":{"message_id":"om-1"}}}`, "om-1"},
		{"qq d.id", "qq", `{"t":"C2C_MESSAGE_CREATE","d":{"id":"qq-1"}}`, "qq-1"},
		{"telegram update_id", "telegram", `{"update_id":12345}`, "update:12345"},
		{"no id", "webhook", `{"content":"hello"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var payload map[string]interface{}
			if err := json.Unmarshal([]byte(tc.payload), &payload); err != nil {
				t.Fatal(err)
			}
			if got := imMessageID(tc.platform, payload); got != tc.want {
				t.Fatalf("imMessageID(%s) = %q, want %q", tc.platform, got, tc.want)
			}
		})
	}
}

// TestIMWebhookDuplicateAcknowledgedWithoutProcessing is the redelivery
// contract: a webhook whose message id was already seen is acknowledged 200
// (so the platform stops retrying) without reaching the agent again.
func TestIMWebhookDuplicateAcknowledgedWithoutProcessing(t *testing.T) {
	store := controltest.NewStore(t)
	t.Cleanup(func() { _ = store.Close() })
	daemon := &Server{Control: store, DefaultTenantID: "default"}

	body := `{"header":{"event_id":"ev-dup-1"},"event":{"message":{"message_id":"om-x","chat_id":"c1","content":"{\"text\":\"hi\"}"}}}`
	identity, err := store.ResolveOrCreateAccount(context.Background(), "default", "feishu", "local", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginInbound(context.Background(), "feishu", "ev-dup-1", []byte(body),
		control.InboundOwner{TenantID: identity.TenantID, PersonID: identity.PersonID}); err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimInbound(context.Background(), "feishu", "ev-dup-1")
	if err != nil || claim == 0 {
		t.Fatalf("claim = %d, %v", claim, err)
	}
	if err := store.AcceptInbound(context.Background(), "feishu", "ev-dup-1", claim); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/im/feishu", strings.NewReader(body))
	rec := httptest.NewRecorder()
	daemon.handleIMWebhook(rec, req)

	if rec.Code != 200 {
		t.Fatalf("duplicate must be acknowledged 200, got %d", rec.Code)
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["status"] != "duplicate" {
		t.Fatalf("expected duplicate acknowledgment, got %v", resp)
	}
}

func TestIMWebhookRetriesWhenDedupStorageIsUnavailable(t *testing.T) {
	store := controltest.NewStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	daemon := &Server{Control: store, DefaultTenantID: "default"}
	body := `{"header":{"event_id":"ev-unrecorded"},"event":{"message":{"message_id":"om-x","chat_id":"c1","content":"{\"text\":\"start work\"}"}}}`
	req := httptest.NewRequest("POST", "/v1/im/feishu", strings.NewReader(body))
	rec := httptest.NewRecorder()
	daemon.handleIMWebhook(rec, req)
	var reply api.MessageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("refusal body is not a message response: %q", rec.Body.String())
	}
	if rec.Code != 503 || !strings.Contains(reply.Error, "not started") || !strings.Contains(reply.Error, "retried automatically") {
		t.Fatalf("unrecorded webhook was acknowledged: code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestIMWebhookReplaysOnlyBeforeDispatch(t *testing.T) {
	store := controltest.NewStore(t)
	t.Cleanup(func() { _ = store.Close() })
	daemon := &Server{Control: store, DefaultTenantID: "default"}
	body := []byte(`{"message_id":"in-1","content":""}`)
	identity, err := store.ResolveOrCreateAccount(context.Background(), "default", "webhook", "local", "")
	if err != nil {
		t.Fatal(err)
	}
	owner := control.InboundOwner{TenantID: identity.TenantID, PersonID: identity.PersonID}
	state, err := store.BeginInbound(context.Background(), "webhook", "in-1", body, owner)
	if err != nil || state != control.InboundPending {
		t.Fatalf("durable pre-dispatch receipt = %q, %v", state, err)
	}
	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/im/webhook", strings.NewReader(string(body)))
		rec := httptest.NewRecorder()
		daemon.handleIMWebhook(rec, req)
		return rec
	}
	first := request()
	if first.Code != 400 {
		t.Fatalf("pending input was not handled: %d %s", first.Code, first.Body.String())
	}
	state, _, err = store.InboundReceipt(context.Background(), "webhook", "in-1")
	if err != nil || state != control.InboundAccepted {
		t.Fatalf("handled input state = %q, %v", state, err)
	}
	if replay := request(); replay.Code != 200 || !strings.Contains(replay.Body.String(), "duplicate") {
		t.Fatalf("handled input replay = %d %s", replay.Code, replay.Body.String())
	}

	uncertain := []byte(`{"message_id":"in-2","content":"do work"}`)
	if _, err := store.BeginInbound(context.Background(), "webhook", "in-2", uncertain, owner); err != nil {
		t.Fatal(err)
	}
	if claim, err := store.ClaimInbound(context.Background(), "webhook", "in-2"); err != nil || claim == 0 {
		t.Fatalf("claim = %d, %v", claim, err)
	}
	req := httptest.NewRequest("POST", "/v1/im/webhook", strings.NewReader(string(uncertain)))
	rec := httptest.NewRecorder()
	daemon.handleIMWebhook(rec, req)
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "uncertain") {
		t.Fatalf("uncertain input was blindly replayed: %d %s", rec.Code, rec.Body.String())
	}
}

// A webhook refused before admission returns its receipt to pending and asks
// the platform to redeliver; the redelivery after restart runs once and a
// further copy is acknowledged as a duplicate.
func TestIMWebhookRefusedBeforeAdmissionStaysRedeliverable(t *testing.T) {
	store := controltest.NewStore(t)
	body := `{"message_id":"in-drain","user_id":"im-owner","chat_id":"im-chat","content":"start the release"}`
	deliver := func(daemon *Server) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/im/webhook", strings.NewReader(body))
		rec := httptest.NewRecorder()
		daemon.handleIMWebhook(rec, req)
		return rec
	}
	draining := newInboundTestServer(store, newSlowLLMProvider("unused"))
	draining.beginDraining("signal terminated")
	refused := deliver(draining)
	var reply api.MessageResponse
	if err := json.Unmarshal(refused.Body.Bytes(), &reply); err != nil {
		t.Fatalf("refusal body is not a message response: %q", refused.Body.String())
	}
	if refused.Code != 503 || !strings.Contains(reply.Error, "shutting down") || !strings.Contains(reply.Error, "retried automatically") {
		t.Fatalf("refusal = %d %+v; want 503 with cause and retry hint", refused.Code, reply)
	}
	if state, _, err := store.InboundReceipt(context.Background(), "webhook", "in-drain"); err != nil || state != control.InboundPending {
		t.Fatalf("refused webhook receipt = %q, %v; want pending", state, err)
	}

	provider := newSlowLLMProvider("release started")
	provider.releaseNow()
	restarted := newInboundTestServer(store, provider)
	if accepted := deliver(restarted); accepted.Code != 200 || !strings.Contains(accepted.Body.String(), "release started") {
		t.Fatalf("redelivery after restart = %d %s", accepted.Code, accepted.Body.String())
	}
	if state, _, err := store.InboundReceipt(context.Background(), "webhook", "in-drain"); err != nil || state != control.InboundAccepted {
		t.Fatalf("redelivered webhook receipt = %q, %v; want accepted", state, err)
	}
	if duplicate := deliver(restarted); duplicate.Code != 200 || !strings.Contains(duplicate.Body.String(), "duplicate") {
		t.Fatalf("second redelivery = %d %s; want duplicate", duplicate.Code, duplicate.Body.String())
	}
}

func TestIMWebhookRequiresStableIDForWork(t *testing.T) {
	store := controltest.NewStore(t)
	t.Cleanup(func() { _ = store.Close() })
	daemon := &Server{Control: store, DefaultTenantID: "default"}
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"content":"do work"}`, 422},
		{`{"content":""}`, 200},
	} {
		req := httptest.NewRequest("POST", "/v1/im/webhook", strings.NewReader(tc.body))
		rec := httptest.NewRecorder()
		daemon.handleIMWebhook(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("idless input %s returned %d, want %d", tc.body, rec.Code, tc.want)
		}
	}
}

// A webhook redelivery may change its timestamp, retry counter, or display
// name; those keep it the same message. A different attachment — a path, a
// media path, or an unnamed URL — or a different tenant, task, workspace, or
// reply target makes it a different message.
func TestWebhookEssentialSeparatesMediaAndRouting(t *testing.T) {
	essential := webhookEssential("telegram")
	digest := func(mutate func(map[string]interface{})) string {
		t.Helper()
		payload := map[string]interface{}{
			"message_id": "in-1", "user_id": "im-owner", "chat_id": "im-chat", "content": "review these",
			"attachments": []interface{}{"/tmp/a.png", map[string]interface{}{"url": "https://files.example/a"}},
			"media_paths": []interface{}{"/tmp/c.png"},
			"timestamp":   1, "retry_count": 0, "display_name": "Owner",
		}
		if mutate != nil {
			mutate(payload)
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		out, err := essential(raw)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	base := digest(nil)
	for name, mutate := range map[string]func(map[string]interface{}){
		"timestamp":    func(p map[string]interface{}) { p["timestamp"] = 2 },
		"retry":        func(p map[string]interface{}) { p["retry_count"] = 3 },
		"display name": func(p map[string]interface{}) { p["display_name"] = "Owner (phone)" },
	} {
		if got := digest(mutate); got != base {
			t.Fatalf("%s change made a different message: %s vs %s", name, got, base)
		}
	}
	for name, mutate := range map[string]func(map[string]interface{}){
		"attachment path": func(p map[string]interface{}) {
			p["attachments"] = []interface{}{"/tmp/b.png", map[string]interface{}{"url": "https://files.example/a"}}
		},
		"attachment url": func(p map[string]interface{}) {
			p["attachments"] = []interface{}{"/tmp/a.png", map[string]interface{}{"url": "https://files.example/b"}}
		},
		"media path": func(p map[string]interface{}) { p["media_paths"] = []interface{}{"/tmp/d.png"} },
		"tenant":     func(p map[string]interface{}) { p["tenant_id"] = "other-tenant" },
		"task":       func(p map[string]interface{}) { p["task_id"] = "task-other" },
		"workspace":  func(p map[string]interface{}) { p["workspace_id"] = "ws-other" },
		"reply": func(p map[string]interface{}) {
			p["message"] = map[string]interface{}{"reply_to_message": map[string]interface{}{"message_id": "77"}}
		},
	} {
		if got := digest(mutate); got == base {
			t.Fatalf("%s change still matched the original message", name)
		}
	}
}
