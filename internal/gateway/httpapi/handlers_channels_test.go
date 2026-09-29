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
	if claimed, err := store.ClaimInbound(context.Background(), "feishu", "ev-dup-1"); err != nil || !claimed {
		t.Fatalf("claim = %t, %v", claimed, err)
	}
	if err := store.AcceptInbound(context.Background(), "feishu", "ev-dup-1"); err != nil {
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
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "retry delivery") {
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
	if claimed, err := store.ClaimInbound(context.Background(), "webhook", "in-2"); err != nil || !claimed {
		t.Fatalf("claimed = %t, %v", claimed, err)
	}
	req := httptest.NewRequest("POST", "/v1/im/webhook", strings.NewReader(string(uncertain)))
	rec := httptest.NewRecorder()
	daemon.handleIMWebhook(rec, req)
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "uncertain") {
		t.Fatalf("uncertain input was blindly replayed: %d %s", rec.Code, rec.Body.String())
	}
}
