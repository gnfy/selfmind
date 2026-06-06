package modelruntime

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Credential struct {
	Token     string
	Source    string
	ExpiresAt time.Time
}

// CredentialStore is SelfMind's own optional credential file. It is separate
// from config.yaml so users can keep secrets out of reusable profile config.
type CredentialStore struct {
	path string
}

func NewCredentialStore(path string) *CredentialStore {
	return &CredentialStore{path: expandHome(path)}
}

// Resolve accepts both the new providers map and the early flat shape to avoid
// breaking local installs while the auth story is still evolving.
func (s *CredentialStore) Resolve(provider string) Credential {
	if s == nil || strings.TrimSpace(s.path) == "" {
		return Credential{}
	}
	payload, err := readJSONFile(s.path)
	if err != nil {
		return Credential{}
	}
	providers, _ := payload["providers"].(map[string]interface{})
	entry, _ := providers[NormalizeProviderID(provider)].(map[string]interface{})
	if entry == nil {
		entry, _ = payload[NormalizeProviderID(provider)].(map[string]interface{})
	}
	if entry == nil {
		return Credential{}
	}
	token := firstToken(entry, "api_key", "apiKey", "access_token", "accessToken", "token")
	if token == "" {
		return Credential{}
	}
	return Credential{Token: token, Source: "selfmind-auth:" + s.path}
}

type ExternalCredentialResolver struct{}

// Resolve performs best-effort reuse for selected coding CLIs only. It never
// refreshes OAuth tokens; the source CLI remains the owner of login lifecycle.
func (ExternalCredentialResolver) Resolve(source string) Credential {
	switch NormalizeProviderID(source) {
	case "codex-cli":
		return resolveCodexCLI()
	case "claude-code":
		return resolveClaudeCode()
	case "gemini-cli", "google-gemini-cli":
		return resolveGeminiCLI()
	case "qwen-cli", "qwen-oauth":
		return resolveQwenCLI()
	default:
		return Credential{}
	}
}

func resolveCodexCLI() Credential {
	if token := strings.TrimSpace(os.Getenv("CODEX_ACCESS_TOKEN")); token != "" {
		return Credential{Token: token, Source: "env:CODEX_ACCESS_TOKEN"}
	}
	for _, path := range []string{
		filepath.Join(firstNonEmpty(os.Getenv("CODEX_HOME"), filepath.Join(homeDir(), ".codex")), "auth.json"),
		filepath.Join(homeDir(), ".codex", "auth.json"),
	} {
		if token := tokenFromJSONFile(path); token != "" {
			return Credential{Token: token, Source: path}
		}
	}
	return Credential{}
}

func resolveClaudeCode() Credential {
	for _, env := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_TOKEN"} {
		if token := strings.TrimSpace(os.Getenv(env)); token != "" {
			return Credential{Token: token, Source: "env:" + env}
		}
	}
	for _, path := range []string{
		filepath.Join(homeDir(), ".claude", ".credentials.json"),
		filepath.Join(homeDir(), ".claude.json"),
		filepath.Join(homeDir(), ".config", "claude", "credentials.json"),
	} {
		if token := tokenFromJSONFile(path); token != "" {
			return Credential{Token: token, Source: path}
		}
	}
	return Credential{}
}

func resolveGeminiCLI() Credential {
	for _, env := range []string{"GEMINI_OAUTH_ACCESS_TOKEN", "GOOGLE_OAUTH_ACCESS_TOKEN"} {
		if token := strings.TrimSpace(os.Getenv(env)); token != "" {
			return Credential{Token: token, Source: "env:" + env}
		}
	}
	for _, path := range []string{
		filepath.Join(homeDir(), ".gemini", "oauth_creds.json"),
		filepath.Join(homeDir(), ".gemini", "credentials.json"),
		filepath.Join(homeDir(), ".config", "gemini", "oauth_creds.json"),
		filepath.Join(homeDir(), ".config", "gcloud", "application_default_credentials.json"),
	} {
		if token := tokenFromJSONFile(path); token != "" {
			return Credential{Token: token, Source: path}
		}
	}
	return Credential{}
}

func resolveQwenCLI() Credential {
	for _, env := range []string{"QWEN_ACCESS_TOKEN", "QWEN_API_KEY"} {
		if token := strings.TrimSpace(os.Getenv(env)); token != "" {
			return Credential{Token: token, Source: "env:" + env}
		}
	}
	for _, path := range []string{
		filepath.Join(homeDir(), ".qwen", "oauth_creds.json"),
		filepath.Join(homeDir(), ".qwen", "credentials.json"),
	} {
		if token := tokenFromJSONFile(path); token != "" {
			return Credential{Token: token, Source: path}
		}
	}
	return Credential{}
}

func tokenFromJSONFile(path string) string {
	payload, err := readJSONFile(path)
	if err != nil {
		return ""
	}
	return firstToken(payload,
		"access_token", "accessToken", "api_key", "apiKey", "id_token", "idToken",
		"token", "auth_token", "authToken",
	)
}

func readJSONFile(path string) (map[string]interface{}, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("empty path")
	}
	data, err := os.ReadFile(expandHome(path))
	if err != nil {
		return nil, err
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func firstToken(value interface{}, keys ...string) string {
	// External CLI credential files are not standardized, so this walks nested
	// JSON and returns the first token-like field from an allowlist.
	keyset := make(map[string]bool, len(keys))
	for _, key := range keys {
		keyset[strings.ToLower(key)] = true
	}
	var walk func(interface{}) string
	walk = func(v interface{}) string {
		switch item := v.(type) {
		case map[string]interface{}:
			for key, child := range item {
				if keyset[strings.ToLower(key)] {
					if token, ok := child.(string); ok && strings.TrimSpace(token) != "" {
						return strings.TrimSpace(token)
					}
				}
			}
			for _, child := range item {
				if token := walk(child); token != "" {
					return token
				}
			}
		case []interface{}:
			for _, child := range item {
				if token := walk(child); token != "" {
					return token
				}
			}
		}
		return ""
	}
	return walk(value)
}

func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "."
	}
	return home
}

func expandHome(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if path == "~" {
		return homeDir()
	}
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		return filepath.Join(homeDir(), path[2:])
	}
	return path
}
