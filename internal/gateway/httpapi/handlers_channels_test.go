package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"testing"
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
