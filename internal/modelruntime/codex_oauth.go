package modelruntime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	CodexOAuthClientID          = "app_EMoamEEZ73f0CkXaXp7hrann"
	CodexOAuthRefreshSkewSecond = 60
)

var codexOAuthTokenEndpoint = "https://auth.openai.com/oauth/token"

func codexCredentialFromFile(path string) Credential {
	path = expandHome(path)
	state, err := readJSONFile(path)
	if err != nil {
		return Credential{}
	}
	token, expiresAt := codexAccessToken(state)
	refreshToken := codexRefreshToken(state)
	if token == "" && refreshToken == "" {
		return Credential{}
	}
	var mu sync.Mutex
	refresher := func() string {
		mu.Lock()
		defer mu.Unlock()
		nextState, err := readJSONFile(path)
		if err == nil {
			state = nextState
		}
		if codexRefreshToken(state) == "" {
			return ""
		}
		refreshed, err := refreshCodexOAuthState(path, state)
		if err != nil {
			return ""
		}
		state = refreshed
		token, _ := codexAccessToken(state)
		return token
	}
	getter := func() string {
		mu.Lock()
		defer mu.Unlock()
		nextState, err := readJSONFile(path)
		if err == nil {
			state = nextState
		}
		token, expiresAt := codexAccessToken(state)
		if shouldRefreshCodexToken(token, expiresAt) && codexRefreshToken(state) != "" {
			if refreshed, err := refreshCodexOAuthState(path, state); err == nil {
				state = refreshed
				token, expiresAt = codexAccessToken(state)
			}
		}
		_ = expiresAt
		return token
	}
	if shouldRefreshCodexToken(token, expiresAt) && refreshToken != "" {
		if refreshed, err := refreshCodexOAuthState(path, state); err == nil {
			state = refreshed
			token, expiresAt = codexAccessToken(state)
		}
	}
	if token == "" {
		return Credential{}
	}
	return Credential{Token: token, Source: path, ExpiresAt: expiresAt, Getter: getter, Refresher: refresher, AccountID: codexAccountID(state)}
}

// codexAccountID extracts the ChatGPT account id from auth.json. The
// chatgpt.com Codex backend requires it as the chatgpt-account-id header;
// omitting it can make the server drop the connection (EOF).
func codexAccountID(state map[string]interface{}) string {
	if tokens, ok := state["tokens"].(map[string]interface{}); ok {
		if id := stringValue(tokens["account_id"]); id != "" {
			return id
		}
	}
	return stringValue(state["account_id"])
}

func codexAccessToken(state map[string]interface{}) (string, time.Time) {
	tokens, _ := state["tokens"].(map[string]interface{})
	token := firstNonEmpty(
		stringValue(tokens["access_token"]),
		stringValue(state["access_token"]),
	)
	return token, jwtExpiresAt(token)
}

func codexRefreshToken(state map[string]interface{}) string {
	tokens, _ := state["tokens"].(map[string]interface{})
	return firstNonEmpty(
		stringValue(tokens["refresh_token"]),
		stringValue(state["refresh_token"]),
	)
}

func shouldRefreshCodexToken(token string, expiresAt time.Time) bool {
	if strings.TrimSpace(token) == "" {
		return true
	}
	if expiresAt.IsZero() {
		return false
	}
	return time.Until(expiresAt) <= CodexOAuthRefreshSkewSecond*time.Second
}

func refreshCodexOAuthState(authPath string, state map[string]interface{}) (map[string]interface{}, error) {
	refreshToken := codexRefreshToken(state)
	if refreshToken == "" {
		return nil, fmt.Errorf("Codex CLI auth state has no refresh_token")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {CodexOAuthClientID},
		"refresh_token": {refreshToken},
	}
	payload, err := codexPostOAuthForm(context.Background(), form)
	if err != nil {
		return nil, err
	}
	accessToken := stringValue(payload["access_token"])
	if accessToken == "" {
		return nil, fmt.Errorf("Codex OAuth refresh response missing access_token")
	}
	next := copyMap(state)
	tokens, _ := next["tokens"].(map[string]interface{})
	if tokens == nil {
		tokens = map[string]interface{}{}
		next["tokens"] = tokens
	}
	tokens["access_token"] = accessToken
	if idToken := stringValue(payload["id_token"]); idToken != "" {
		tokens["id_token"] = idToken
	}
	if nextRefresh := stringValue(payload["refresh_token"]); nextRefresh != "" {
		tokens["refresh_token"] = nextRefresh
	} else {
		tokens["refresh_token"] = refreshToken
	}
	if tokenType := stringValue(payload["token_type"]); tokenType != "" {
		tokens["token_type"] = tokenType
	}
	if expiresIn := int64Value(payload["expires_in"]); expiresIn > 0 {
		tokens["expires_in"] = expiresIn
		tokens["expires_at"] = time.Now().UTC().Add(time.Duration(expiresIn) * time.Second).Format(time.RFC3339Nano)
	}
	next["last_refresh"] = time.Now().UTC().Format(time.RFC3339Nano)
	if err := writeAuthStore(authPath, next); err != nil {
		return nil, err
	}
	return next, nil
}

func codexPostOAuthForm(ctx context.Context, form url.Values) (map[string]interface{}, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, codexOAuthTokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("Codex OAuth HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload map[string]interface{}
	if len(body) == 0 {
		return map[string]interface{}{}, nil
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func jwtExpiresAt(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims map[string]interface{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}
	}
	exp := int64Value(claims["exp"])
	if exp <= 0 {
		return time.Time{}
	}
	return time.Unix(exp, 0).UTC()
}
