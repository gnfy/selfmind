package modelruntime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	MiniMaxOAuthProvider          = "minimax-oauth"
	MiniMaxOAuthClientID          = "78257093-7e40-4613-99e0-527b14b39113"
	MiniMaxOAuthScope             = "group_id profile model.completion"
	MiniMaxOAuthGrantType         = "urn:ietf:params:oauth:grant-type:user_code"
	MiniMaxOAuthGlobalPortalBase  = "https://api.minimax.io"
	MiniMaxOAuthCNPortalBase      = "https://api.minimaxi.com"
	MiniMaxOAuthGlobalInference   = "https://api.minimax.io/anthropic"
	MiniMaxOAuthCNInference       = "https://api.minimaxi.com/anthropic"
	MiniMaxOAuthRefreshSkewSecond = 60
)

type MiniMaxOAuthLoginOptions struct {
	Region      string
	OpenBrowser bool
}

type MiniMaxOAuthStatus struct {
	LoggedIn           bool
	Provider           string
	Region             string
	PortalBaseURL      string
	InferenceBaseURL   string
	ExpiresAt          time.Time
	HasRefreshToken    bool
	LastAuthError      string
	CredentialFilePath string
}

func (s *CredentialStore) ResolveMiniMaxOAuth() Credential {
	if s == nil || strings.TrimSpace(s.path) == "" {
		return Credential{}
	}
	token, err := s.currentMiniMaxOAuthToken()
	if err != nil || token == "" {
		return Credential{}
	}
	return Credential{
		Token:  token,
		Source: "selfmind-auth:" + s.path,
		Getter: func() string {
			token, err := s.currentMiniMaxOAuthToken()
			if err != nil {
				return ""
			}
			return token
		},
		Refresher: func() string {
			token, err := s.refreshMiniMaxOAuthToken()
			if err != nil {
				return ""
			}
			return token
		},
	}
}

func (s *CredentialStore) currentMiniMaxOAuthToken() (string, error) {
	state, err := readProviderState(s.path, MiniMaxOAuthProvider)
	if err != nil {
		return "", err
	}
	token := stringValue(state["access_token"])
	if token == "" {
		return "", fmt.Errorf("not logged into MiniMax OAuth")
	}
	expiresAt := parseMiniMaxExpiry(stringValue(state["expires_at"]))
	if expiresAt.IsZero() || time.Until(expiresAt) <= MiniMaxOAuthRefreshSkewSecond*time.Second {
		state, err = refreshMiniMaxOAuthState(s.path, state)
		if err != nil {
			quarantineMiniMaxOAuthState(s.path, state, err)
			return "", err
		}
		token = stringValue(state["access_token"])
	}
	if token == "" {
		return "", fmt.Errorf("MiniMax OAuth state has no access token")
	}
	return token, nil
}

func (s *CredentialStore) refreshMiniMaxOAuthToken() (string, error) {
	state, err := readProviderState(s.path, MiniMaxOAuthProvider)
	if err != nil {
		return "", err
	}
	state, err = refreshMiniMaxOAuthState(s.path, state)
	if err != nil {
		quarantineMiniMaxOAuthState(s.path, state, err)
		return "", err
	}
	token := stringValue(state["access_token"])
	if token == "" {
		return "", fmt.Errorf("MiniMax OAuth refresh returned no access token")
	}
	return token, nil
}

func LoginMiniMaxOAuth(ctx context.Context, authPath string, opts MiniMaxOAuthLoginOptions, out io.Writer) (MiniMaxOAuthStatus, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if out == nil {
		out = io.Discard
	}
	authPath = expandHome(firstNonEmpty(authPath, filepath.Join(homeDir(), ".selfmind", "auth.json")))
	region := strings.ToLower(strings.TrimSpace(opts.Region))
	if region == "" {
		region = "global"
	}
	portalBase, inferenceBase := miniMaxOAuthEndpoints(region)
	verifier, challenge, state, err := miniMaxPKCEPair()
	if err != nil {
		return MiniMaxOAuthStatus{}, err
	}

	fmt.Fprintf(out, "Starting MiniMax OAuth login (%s)\n", region)
	codeData, err := miniMaxRequestUserCode(ctx, portalBase, challenge, state)
	if err != nil {
		return MiniMaxOAuthStatus{}, err
	}
	verificationURL := stringValue(codeData["verification_uri"])
	userCode := stringValue(codeData["user_code"])
	fmt.Fprintln(out, "Open this URL:")
	fmt.Fprintf(out, "  %s\n", verificationURL)
	if userCode != "" {
		fmt.Fprintf(out, "Enter code if prompted: %s\n", userCode)
	}
	if opts.OpenBrowser && verificationURL != "" {
		_ = openBrowser(verificationURL)
	}
	fmt.Fprintln(out, "Waiting for MiniMax authorization...")

	tokenData, err := miniMaxPollToken(ctx, portalBase, userCode, verifier, int64Value(codeData["expired_in"]), int64Value(codeData["interval"]))
	if err != nil {
		return MiniMaxOAuthStatus{}, err
	}
	now := time.Now().UTC()
	expiresAt := miniMaxResolveTokenExpiry(int64Value(tokenData["expired_in"]), now)
	authState := map[string]interface{}{
		"provider":           MiniMaxOAuthProvider,
		"region":             region,
		"portal_base_url":    portalBase,
		"inference_base_url": inferenceBase,
		"client_id":          MiniMaxOAuthClientID,
		"scope":              MiniMaxOAuthScope,
		"token_type":         firstNonEmpty(stringValue(tokenData["token_type"]), "Bearer"),
		"access_token":       stringValue(tokenData["access_token"]),
		"refresh_token":      stringValue(tokenData["refresh_token"]),
		"resource_url":       stringValue(tokenData["resource_url"]),
		"obtained_at":        now.Format(time.RFC3339Nano),
		"expires_at":         expiresAt.Format(time.RFC3339Nano),
		"expires_in":         int(time.Until(expiresAt).Seconds()),
	}
	if authState["access_token"] == "" || authState["refresh_token"] == "" {
		return MiniMaxOAuthStatus{}, fmt.Errorf("MiniMax OAuth token response missing access_token or refresh_token")
	}
	if err := writeProviderState(authPath, MiniMaxOAuthProvider, authState); err != nil {
		return MiniMaxOAuthStatus{}, err
	}
	fmt.Fprintln(out, "MiniMax OAuth login successful.")
	return MiniMaxOAuthAuthStatus(authPath)
}

func MiniMaxOAuthAuthStatus(authPath string) (MiniMaxOAuthStatus, error) {
	authPath = expandHome(firstNonEmpty(authPath, filepath.Join(homeDir(), ".selfmind", "auth.json")))
	state, err := readProviderState(authPath, MiniMaxOAuthProvider)
	if err != nil {
		return MiniMaxOAuthStatus{Provider: MiniMaxOAuthProvider, CredentialFilePath: authPath}, nil
	}
	expiresAt := parseMiniMaxExpiry(stringValue(state["expires_at"]))
	return MiniMaxOAuthStatus{
		LoggedIn:           stringValue(state["access_token"]) != "" && (expiresAt.IsZero() || time.Now().Before(expiresAt)),
		Provider:           MiniMaxOAuthProvider,
		Region:             stringValue(state["region"]),
		PortalBaseURL:      stringValue(state["portal_base_url"]),
		InferenceBaseURL:   stringValue(state["inference_base_url"]),
		ExpiresAt:          expiresAt,
		HasRefreshToken:    stringValue(state["refresh_token"]) != "",
		LastAuthError:      lastAuthErrorText(state["last_auth_error"]),
		CredentialFilePath: authPath,
	}, nil
}

func LogoutMiniMaxOAuth(authPath string) error {
	authPath = expandHome(firstNonEmpty(authPath, filepath.Join(homeDir(), ".selfmind", "auth.json")))
	return deleteProviderState(authPath, MiniMaxOAuthProvider)
}

func miniMaxOAuthEndpoints(region string) (string, string) {
	switch strings.ToLower(strings.TrimSpace(region)) {
	case "cn", "china", "minimax-cn":
		return MiniMaxOAuthCNPortalBase, MiniMaxOAuthCNInference
	default:
		return MiniMaxOAuthGlobalPortalBase, MiniMaxOAuthGlobalInference
	}
}

func miniMaxPKCEPair() (verifier, challenge, state string, err error) {
	verifier, err = randomURLSafe(72)
	if err != nil {
		return "", "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	state, err = randomURLSafe(18)
	if err != nil {
		return "", "", "", err
	}
	return verifier, challenge, state, nil
}

func randomURLSafe(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func miniMaxRequestUserCode(ctx context.Context, portalBase, challenge, state string) (map[string]interface{}, error) {
	form := url.Values{
		"response_type":         {"code"},
		"client_id":             {MiniMaxOAuthClientID},
		"scope":                 {MiniMaxOAuthScope},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	payload, err := miniMaxPostForm(ctx, portalBase+"/oauth/code", form)
	if err != nil {
		return nil, err
	}
	for _, field := range []string{"user_code", "verification_uri", "expired_in"} {
		if payload[field] == nil {
			return nil, fmt.Errorf("MiniMax OAuth response missing field %s", field)
		}
	}
	if got := stringValue(payload["state"]); got != "" && got != state {
		return nil, fmt.Errorf("MiniMax OAuth state mismatch")
	}
	return payload, nil
}

func miniMaxPollToken(ctx context.Context, portalBase, userCode, verifier string, expiredIn, intervalRaw int64) (map[string]interface{}, error) {
	now := time.Now()
	deadline := miniMaxResolveTokenExpiry(expiredIn, now)
	interval := time.Duration(intervalRaw) * time.Millisecond
	if interval < 2*time.Second {
		interval = 2 * time.Second
	}
	for time.Now().Before(deadline) {
		form := url.Values{
			"grant_type":    {MiniMaxOAuthGrantType},
			"client_id":     {MiniMaxOAuthClientID},
			"user_code":     {userCode},
			"code_verifier": {verifier},
		}
		payload, err := miniMaxPostForm(ctx, portalBase+"/oauth/token", form)
		if err != nil {
			return nil, err
		}
		switch strings.ToLower(stringValue(payload["status"])) {
		case "success":
			if stringValue(payload["access_token"]) == "" || stringValue(payload["refresh_token"]) == "" {
				return nil, fmt.Errorf("MiniMax OAuth success payload missing token fields")
			}
			return payload, nil
		case "error":
			return nil, fmt.Errorf("MiniMax OAuth reported an authorization error")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
	return nil, fmt.Errorf("MiniMax OAuth timed out before authorization completed")
}

func refreshMiniMaxOAuthState(authPath string, state map[string]interface{}) (map[string]interface{}, error) {
	refreshToken := stringValue(state["refresh_token"])
	if refreshToken == "" {
		return nil, fmt.Errorf("MiniMax OAuth state has no refresh_token")
	}
	portalBase := firstNonEmpty(stringValue(state["portal_base_url"]), MiniMaxOAuthGlobalPortalBase)
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {firstNonEmpty(stringValue(state["client_id"]), MiniMaxOAuthClientID)},
		"refresh_token": {refreshToken},
	}
	payload, err := miniMaxPostForm(context.Background(), portalBase+"/oauth/token", form)
	if err != nil {
		return nil, err
	}
	if strings.ToLower(stringValue(payload["status"])) != "success" {
		return nil, fmt.Errorf("MiniMax OAuth refresh did not return success")
	}
	now := time.Now().UTC()
	expiresAt := miniMaxResolveTokenExpiry(int64Value(payload["expired_in"]), now)
	next := copyMap(state)
	next["access_token"] = stringValue(payload["access_token"])
	next["refresh_token"] = firstNonEmpty(stringValue(payload["refresh_token"]), refreshToken)
	next["obtained_at"] = now.Format(time.RFC3339Nano)
	next["expires_at"] = expiresAt.Format(time.RFC3339Nano)
	next["expires_in"] = int(time.Until(expiresAt).Seconds())
	if next["access_token"] == "" {
		return nil, fmt.Errorf("MiniMax OAuth refresh response missing access_token")
	}
	if err := writeProviderState(authPath, MiniMaxOAuthProvider, next); err != nil {
		return nil, err
	}
	return next, nil
}

func quarantineMiniMaxOAuthState(authPath string, state map[string]interface{}, cause error) {
	if len(state) == 0 {
		return
	}
	for _, key := range []string{"access_token", "refresh_token", "expires_at", "expires_in", "obtained_at"} {
		delete(state, key)
	}
	state["last_auth_error"] = map[string]interface{}{
		"provider":         MiniMaxOAuthProvider,
		"message":          cause.Error(),
		"relogin_required": true,
		"at":               time.Now().UTC().Format(time.RFC3339Nano),
	}
	_ = writeProviderState(authPath, MiniMaxOAuthProvider, state)
}

func miniMaxPostForm(ctx context.Context, endpoint string, form url.Values) (map[string]interface{}, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
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
		return nil, fmt.Errorf("MiniMax OAuth HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
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

func miniMaxResolveTokenExpiry(expiredIn int64, now time.Time) time.Time {
	if expiredIn <= 0 {
		return now.Add(time.Hour)
	}
	if expiredIn > now.UnixMilli()/2 {
		return time.UnixMilli(expiredIn).UTC()
	}
	return now.Add(time.Duration(expiredIn) * time.Second).UTC()
}

func parseMiniMaxExpiry(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t
	}
	return time.Time{}
}

func readProviderState(path, provider string) (map[string]interface{}, error) {
	store, err := readAuthStore(path)
	if err != nil {
		return nil, err
	}
	providers, _ := store["providers"].(map[string]interface{})
	if providers == nil {
		return nil, fmt.Errorf("auth store has no providers")
	}
	state, _ := providers[NormalizeProviderID(provider)].(map[string]interface{})
	if state == nil {
		return nil, fmt.Errorf("provider %s not found in auth store", provider)
	}
	return state, nil
}

func writeProviderState(path, provider string, state map[string]interface{}) error {
	store, _ := readAuthStore(path)
	if store == nil {
		store = map[string]interface{}{}
	}
	providers, _ := store["providers"].(map[string]interface{})
	if providers == nil {
		providers = map[string]interface{}{}
		store["providers"] = providers
	}
	providers[NormalizeProviderID(provider)] = state
	return writeAuthStore(path, store)
}

func deleteProviderState(path, provider string) error {
	store, err := readAuthStore(path)
	if err != nil {
		return nil
	}
	providers, _ := store["providers"].(map[string]interface{})
	if providers != nil {
		delete(providers, NormalizeProviderID(provider))
	}
	return writeAuthStore(path, store)
}

func readAuthStore(path string) (map[string]interface{}, error) {
	path = expandHome(path)
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("empty auth path")
	}
	payload, err := readJSONFile(path)
	if os.IsNotExist(err) {
		return map[string]interface{}{}, nil
	}
	return payload, err
}

func writeAuthStore(path string, payload map[string]interface{}) error {
	path = expandHome(path)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func openBrowser(rawURL string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("cmd", "/c", "start", "", rawURL).Start()
	case "darwin":
		return exec.Command("open", rawURL).Start()
	default:
		return exec.Command("xdg-open", rawURL).Start()
	}
}

func copyMap(in map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func stringValue(value interface{}) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func int64Value(value interface{}) int64 {
	switch v := value.(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	case string:
		var n int64
		_, _ = fmt.Sscan(strings.TrimSpace(v), &n)
		return n
	default:
		return 0
	}
}

func lastAuthErrorText(value interface{}) string {
	if m, ok := value.(map[string]interface{}); ok {
		return stringValue(m["message"])
	}
	return stringValue(value)
}
