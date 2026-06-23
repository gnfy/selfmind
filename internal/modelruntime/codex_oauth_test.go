package modelruntime

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCodexCredentialRefreshesExpiredAuthJSON(t *testing.T) {
	var refreshCalls int32
	freshToken := fakeCodexJWT(time.Now().Add(time.Hour))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&refreshCalls, 1)
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if got := r.Form.Get("grant_type"); got != "refresh_token" {
			t.Fatalf("grant_type = %q", got)
		}
		if got := r.Form.Get("client_id"); got != CodexOAuthClientID {
			t.Fatalf("client_id = %q", got)
		}
		if got := r.Form.Get("refresh_token"); got != "old-refresh-token" {
			t.Fatalf("refresh_token = %q", got)
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"new-refresh-token","id_token":"new-id-token","token_type":"Bearer","expires_in":3600}`, freshToken)
	}))
	defer server.Close()
	oldEndpoint := codexOAuthTokenEndpoint
	codexOAuthTokenEndpoint = server.URL
	defer func() { codexOAuthTokenEndpoint = oldEndpoint }()

	authPath := filepath.Join(t.TempDir(), "auth.json")
	expiredToken := fakeCodexJWT(time.Now().Add(-time.Hour))
	payload := fmt.Sprintf(`{"auth_mode":"chatgpt","tokens":{"access_token":%q,"refresh_token":"old-refresh-token","id_token":"old-id-token","account_id":"acct-test"},"last_refresh":"2026-01-01T00:00:00Z"}`, expiredToken)
	if err := os.WriteFile(authPath, []byte(payload), 0600); err != nil {
		t.Fatal(err)
	}

	cred := codexCredentialFromFile(authPath)
	if cred.Token != freshToken {
		t.Fatalf("token = %q, want refreshed token", cred.Token)
	}
	if cred.Getter == nil {
		t.Fatal("Getter = nil")
	}
	if got := cred.Getter(); got != freshToken {
		t.Fatalf("Getter() = %q, want refreshed token", got)
	}
	if got := atomic.LoadInt32(&refreshCalls); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}

	updated, err := readJSONFile(authPath)
	if err != nil {
		t.Fatalf("read updated auth: %v", err)
	}
	tokens, _ := updated["tokens"].(map[string]interface{})
	if tokens["access_token"] != freshToken {
		t.Fatalf("stored access_token was not refreshed")
	}
	if tokens["refresh_token"] != "new-refresh-token" {
		t.Fatalf("stored refresh_token = %v", tokens["refresh_token"])
	}
	if tokens["account_id"] != "acct-test" {
		t.Fatalf("account_id should be preserved, got %v", tokens["account_id"])
	}
	if strings.TrimSpace(stringValue(updated["last_refresh"])) == "" {
		t.Fatalf("last_refresh was not updated")
	}
}

func fakeCodexJWT(exp time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	claims := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix())))
	return header + "." + claims + ".sig"
}
