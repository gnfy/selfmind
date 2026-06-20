package modelruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMiniMaxPKCEPair(t *testing.T) {
	verifier, challenge, state, err := miniMaxPKCEPair()
	if err != nil {
		t.Fatal(err)
	}
	if verifier == "" || challenge == "" || state == "" {
		t.Fatalf("empty pkce values verifier=%q challenge=%q state=%q", verifier, challenge, state)
	}
	_, challenge2, state2, err := miniMaxPKCEPair()
	if err != nil {
		t.Fatal(err)
	}
	if challenge == challenge2 || state == state2 {
		t.Fatal("pkce values should be random")
	}
}

func TestMiniMaxResolveTokenExpiry(t *testing.T) {
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	if got := miniMaxResolveTokenExpiry(3600, now); got.Sub(now) != time.Hour {
		t.Fatalf("ttl expiry = %s", got.Sub(now))
	}
	absolute := now.Add(2 * time.Hour).UnixMilli()
	if got := miniMaxResolveTokenExpiry(absolute, now); got.Sub(now) != 2*time.Hour {
		t.Fatalf("absolute expiry = %s", got.Sub(now))
	}
}

func TestMiniMaxOAuthResolveRefreshesExpiredToken(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":        "success",
			"access_token":  "fresh-token",
			"refresh_token": "fresh-refresh",
			"expired_in":    3600,
		})
	}))
	defer server.Close()

	expired := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	payload := fmt.Sprintf(`{"providers":{"minimax-oauth":{"access_token":"old-token","refresh_token":"refresh-token","expires_at":%q,"portal_base_url":%q,"inference_base_url":"https://api.minimax.io/anthropic","region":"global","client_id":%q}}}`, expired, server.URL, MiniMaxOAuthClientID)
	if err := os.WriteFile(authPath, []byte(payload), 0600); err != nil {
		t.Fatal(err)
	}

	cred := NewCredentialStore(authPath).ResolveMiniMaxOAuth()
	if cred.Token != "fresh-token" {
		t.Fatalf("token = %q", cred.Token)
	}
	if got := cred.Getter(); got != "fresh-token" {
		t.Fatalf("getter token = %q", got)
	}
	status, err := MiniMaxOAuthAuthStatus(authPath)
	if err != nil {
		t.Fatal(err)
	}
	if !status.LoggedIn || !status.HasRefreshToken {
		t.Fatalf("status = %+v", status)
	}
}

func TestMiniMaxOAuthRequestAndPoll(t *testing.T) {
	var polls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/oauth/code":
			_ = r.ParseForm()
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"user_code":        "ABC-123",
				"verification_uri": "https://example.test/verify",
				"expired_in":       60,
				"interval":         1,
				"state":            r.Form.Get("state"),
			})
		case "/oauth/token":
			polls++
			if polls == 1 {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "pending"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status":        "success",
				"access_token":  "access-token",
				"refresh_token": "refresh-token",
				"expired_in":    3600,
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	_, challenge, state, err := miniMaxPKCEPair()
	if err != nil {
		t.Fatal(err)
	}
	code, err := miniMaxRequestUserCode(context.Background(), server.URL, challenge, state)
	if err != nil {
		t.Fatal(err)
	}
	token, err := miniMaxPollToken(context.Background(), server.URL, stringValue(code["user_code"]), "verifier", int64Value(code["expired_in"]), int64Value(code["interval"]))
	if err != nil {
		t.Fatal(err)
	}
	if stringValue(token["access_token"]) != "access-token" || polls != 2 {
		t.Fatalf("token=%v polls=%d", token, polls)
	}
}
