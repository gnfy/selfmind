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
	"time"
)

const (
	CodexOAuthClientID          = "app_EMoamEEZ73f0CkXaXp7hrann"
	CodexOAuthRefreshSkewSecond = 60
)

var codexOAuthTokenEndpoint = "https://auth.openai.com/oauth/token"

// codexLoginHint is the actionable message surfaced on a permanent refresh
// failure, instead of raw provider JSON.
const codexLoginHint = "Codex login expired or revoked — run `codex login`, then retry."

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

	// Route refresh through the process-global manager: per-auth-file
	// single-flight (no rotation stampede under a worker pool), quarantine on
	// permanent failure, and an actionable error instead of an empty token.
	ref := AuthRef{Provider: "codex-cli", Kind: AuthOAuthFile, Path: path}
	globalAuthManager.Register(ref, codexLoad, codexRefresh)

	// Resolve the current token now (refreshes if expired), preserving the old
	// resolve-time behavior.
	if resolved, rerr := globalAuthManager.Token(ref); rerr == nil && resolved != "" {
		token = resolved
		if snap, ok := globalAuthManager.snapshot(ref); ok {
			expiresAt = snap.ExpiresAt
		}
	}
	if token == "" {
		return Credential{}
	}
	return Credential{
		Token:     token,
		Source:    path,
		ExpiresAt: expiresAt,
		AccountID: globalAuthManager.AccountID(ref),
		Getter:    func() string { t, _ := globalAuthManager.Token(ref); return t },
		Refresher: func() string { t, _ := globalAuthManager.ForceRefresh(ref); return t },
	}
}

// codexLoad reads the current Codex auth state into a snapshot.
func codexLoad(ref AuthRef) (AuthSnapshot, error) {
	state, err := readJSONFile(ref.Path)
	if err != nil {
		return AuthSnapshot{}, err
	}
	token, expiresAt := codexAccessToken(state)
	return AuthSnapshot{
		Token:        token,
		RefreshToken: codexRefreshToken(state),
		ExpiresAt:    expiresAt,
		AccountID:    codexAccountID(state),
	}, nil
}

// codexRefresh rotates the ChatGPT OAuth token (reusing the proven
// refreshCodexOAuthState) and classifies failures as transient vs permanent.
func codexRefresh(ref AuthRef, _ AuthSnapshot) (AuthSnapshot, *AuthError) {
	state, err := readJSONFile(ref.Path)
	if err != nil {
		return AuthSnapshot{}, &AuthError{Reason: "read_auth_file", Cause: err}
	}
	if codexRefreshToken(state) == "" {
		return AuthSnapshot{}, &AuthError{Permanent: true, Reason: "no_refresh_token", Actionable: codexLoginHint}
	}
	refreshed, rerr := refreshCodexOAuthState(ref.Path, state)
	if rerr != nil {
		return AuthSnapshot{}, classifyCodexRefreshError(rerr)
	}
	token, expiresAt := codexAccessToken(refreshed)
	return AuthSnapshot{
		Token:        token,
		RefreshToken: codexRefreshToken(refreshed),
		ExpiresAt:    expiresAt,
		AccountID:    codexAccountID(refreshed),
	}, nil
}

// classifyCodexRefreshError marks rotation/login failures permanent (quarantine
// + `codex login`) and everything else transient (retryable).
func classifyCodexRefreshError(err error) *AuthError {
	msg := strings.ToLower(err.Error())
	permanent := strings.Contains(msg, "invalid_grant") ||
		strings.Contains(msg, "invalid_client") ||
		(strings.Contains(msg, "refresh_token") &&
			(strings.Contains(msg, "expired") || strings.Contains(msg, "reused") || strings.Contains(msg, "revoked")))
	if permanent {
		return &AuthError{Permanent: true, Reason: "codex_refresh_permanent", Actionable: codexLoginHint, Cause: err}
	}
	return &AuthError{Reason: "codex_refresh_transient", Cause: err}
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
