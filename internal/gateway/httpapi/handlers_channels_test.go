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
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	daemon := &Server{Control: store, DefaultTenantID: "default"}

	// Mark the id as already seen (the first delivery processed it).
	if _, err := store.MarkInboundSeen(context.Background(), "feishu", "ev-dup-1"); err != nil {
		t.Fatal(err)
	}

	body := `{"header":{"event_id":"ev-dup-1"},"event":{"message":{"message_id":"om-x","chat_id":"c1","content":"{\"text\":\"hi\"}"}}}`
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
