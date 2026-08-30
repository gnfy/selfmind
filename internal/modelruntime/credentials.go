package modelruntime

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"selfmind/internal/platform/config"
)

type Credential struct {
	Token     string
	Source    string
	ExpiresAt time.Time
	Getter    func() string
	Refresher func() string
	// AccountID is the provider account identifier (e.g. the ChatGPT
	// account_id for Codex), sent as a required header by some backends.
	AccountID string
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

// ResolveStaged returns a candidate credential without making it active. Stage
// identifiers are opaque and contain no secret material.
func (s *CredentialStore) ResolveStaged(stageID, provider string) Credential {
	if s == nil || strings.TrimSpace(s.path) == "" || strings.TrimSpace(stageID) == "" {
		return Credential{}
	}
	payload, err := readJSONFile(s.path)
	if err != nil {
		return Credential{}
	}
	stage, _ := nestedMap(payload, "staged", strings.TrimSpace(stageID))
	providers, _ := stage["providers"].(map[string]interface{})
	entry, _ := providers[NormalizeProviderID(provider)].(map[string]interface{})
	if entry == nil {
		return Credential{}
	}
	token := firstToken(entry, "api_key")
	if token == "" {
		return Credential{}
	}
	return Credential{Token: token, Source: "selfmind-auth-stage:" + strings.TrimSpace(stageID)}
}

// OverlayStage applies staged credentials to an in-memory candidate config for
// validation. It never writes secrets to config.yaml.
func (s *CredentialStore) OverlayStage(stageID string, cfg *config.Config) error {
	stageID = strings.TrimSpace(stageID)
	if stageID == "" {
		return nil
	}
	if cfg == nil {
		return fmt.Errorf("candidate config is required")
	}
	payload, err := readJSONFile(s.path)
	if err != nil {
		return fmt.Errorf("read credential stage %q: %w", stageID, err)
	}
	stage, ok := nestedMap(payload, "staged", stageID)
	if !ok {
		return fmt.Errorf("credential stage %q was not found", stageID)
	}
	providers, _ := stage["providers"].(map[string]interface{})
	for provider, raw := range providers {
		entry, _ := raw.(map[string]interface{})
		apiKey := firstToken(entry, "api_key")
		if apiKey == "" {
			continue
		}
		if _, custom := cfg.Providers.CustomProvider(provider); custom {
			customID := NormalizeProviderID(strings.TrimPrefix(provider, "custom:"))
			for index := range cfg.Providers.Custom {
				if NormalizeProviderID(cfg.Providers.Custom[index].Name) == customID {
					cfg.Providers.Custom[index].APIKey = apiKey
				}
			}
			continue
		}
		endpoint, _ := cfg.Providers.BuiltinEndpoint(provider)
		endpoint.APIKey = apiKey
		cfg.Providers.SetBuiltinEndpoint(provider, endpoint)
	}
	return nil
}

// StageAPIKeys records candidate secrets separately from the active provider
// map. Reusing an uncommitted stage merges additional provider credentials,
// which lets an interactive manager validate routes one at a time.
func (s *CredentialStore) StageAPIKeys(stageID string, credentials map[string]string) (string, error) {
	if s == nil || strings.TrimSpace(s.path) == "" {
		return "", fmt.Errorf("credential store path is empty")
	}
	clean := make(map[string]string, len(credentials))
	for provider, apiKey := range credentials {
		provider = NormalizeProviderID(provider)
		apiKey = strings.TrimSpace(apiKey)
		if provider != "" && apiKey != "" {
			clean[provider] = apiKey
		}
	}
	if len(clean) == 0 {
		return strings.TrimSpace(stageID), nil
	}
	stageID = strings.TrimSpace(stageID)
	if stageID == "" {
		var err error
		stageID, err = newCredentialStageID()
		if err != nil {
			return "", err
		}
	}
	err := s.mutate(func(payload map[string]interface{}) error {
		staged := ensureMap(payload, "staged")
		cutoff := time.Now().UTC().Add(-24 * time.Hour)
		for id, raw := range staged {
			candidate, _ := raw.(map[string]interface{})
			status, _ := candidate["status"].(string)
			createdAt, _ := candidate["created_at"].(string)
			created, _ := time.Parse(time.RFC3339Nano, createdAt)
			if status != "committed" && !created.IsZero() && created.Before(cutoff) {
				delete(staged, id)
			}
		}
		stage, _ := staged[stageID].(map[string]interface{})
		if stage == nil {
			stage = map[string]interface{}{
				"created_at": time.Now().UTC().Format(time.RFC3339Nano),
				"status":     "staged",
				"providers":  map[string]interface{}{},
			}
			staged[stageID] = stage
		}
		if status, _ := stage["status"].(string); status == "committed" {
			return fmt.Errorf("credential stage %q is already committed", stageID)
		}
		providers := ensureMap(stage, "providers")
		for provider, apiKey := range clean {
			providers[provider] = map[string]interface{}{"api_key": apiKey}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return stageID, nil
}

// CommitStage atomically swaps every staged provider secret into the active
// map while retaining an exact rollback image in the same credential file.
func (s *CredentialStore) CommitStage(stageID string) error {
	stageID = strings.TrimSpace(stageID)
	if stageID == "" {
		return nil
	}
	return s.mutate(func(payload map[string]interface{}) error {
		stage, ok := nestedMap(payload, "staged", stageID)
		if !ok {
			return fmt.Errorf("credential stage %q was not found", stageID)
		}
		if status, _ := stage["status"].(string); status == "committed" {
			return nil
		}
		candidates, _ := stage["providers"].(map[string]interface{})
		if len(candidates) == 0 {
			return fmt.Errorf("credential stage %q is empty", stageID)
		}
		active := ensureMap(payload, "providers")
		previous := make(map[string]interface{}, len(candidates))
		for provider, raw := range candidates {
			if old, exists := active[provider]; exists {
				previous[provider] = cloneJSONValue(old)
			} else {
				previous[provider] = nil
			}
			candidate, _ := raw.(map[string]interface{})
			entry, _ := active[provider].(map[string]interface{})
			if entry == nil {
				entry = map[string]interface{}{}
			} else {
				entry = cloneJSONValue(entry).(map[string]interface{})
			}
			entry["api_key"] = firstToken(candidate, "api_key")
			active[provider] = entry
		}
		stage["previous"] = previous
		stage["status"] = "committed"
		stage["committed_at"] = time.Now().UTC().Format(time.RFC3339Nano)
		return nil
	})
}

// RollbackStage restores the active provider entries captured by CommitStage.
// An uncommitted stage is simply discarded.
func (s *CredentialStore) RollbackStage(stageID string) error {
	stageID = strings.TrimSpace(stageID)
	if stageID == "" {
		return nil
	}
	return s.mutate(func(payload map[string]interface{}) error {
		staged, _ := payload["staged"].(map[string]interface{})
		stage, _ := staged[stageID].(map[string]interface{})
		if stage == nil {
			return nil
		}
		if status, _ := stage["status"].(string); status == "committed" {
			active := ensureMap(payload, "providers")
			previous, _ := stage["previous"].(map[string]interface{})
			for provider, raw := range previous {
				if raw == nil {
					delete(active, provider)
				} else {
					active[provider] = cloneJSONValue(raw)
				}
			}
		}
		delete(staged, stageID)
		if len(staged) == 0 {
			delete(payload, "staged")
		}
		return nil
	})
}

// DiscardStage removes a candidate stage. It is rollback-safe if a caller is
// recovering from a partially completed transaction.
func (s *CredentialStore) DiscardStage(stageID string) error {
	return s.RollbackStage(stageID)
}

// FinalizeStage forgets the rollback image after the replacement daemon has
// reached the verified healthy boundary.
func (s *CredentialStore) FinalizeStage(stageID string) error {
	stageID = strings.TrimSpace(stageID)
	if stageID == "" {
		return nil
	}
	return s.mutate(func(payload map[string]interface{}) error {
		staged, _ := payload["staged"].(map[string]interface{})
		delete(staged, stageID)
		if len(staged) == 0 {
			delete(payload, "staged")
		}
		return nil
	})
}

// SaveAPIKey writes one provider secret to SelfMind's dedicated credential
// file. The file is separate from reusable config, mode 0600, and replaced
// atomically so a daemon can safely resolve it while setup is running.
func (s *CredentialStore) SaveAPIKey(provider, apiKey string) error {
	if s == nil || strings.TrimSpace(s.path) == "" {
		return fmt.Errorf("credential store path is empty")
	}
	provider = NormalizeProviderID(provider)
	if provider == "" {
		return fmt.Errorf("credential provider is required")
	}
	return s.mutate(func(payload map[string]interface{}) error {
		providers := ensureMap(payload, "providers")
		entry, _ := providers[provider].(map[string]interface{})
		if entry == nil {
			entry = map[string]interface{}{}
		}
		apiKey = strings.TrimSpace(apiKey)
		if apiKey == "" {
			delete(entry, "api_key")
		} else {
			entry["api_key"] = apiKey
		}
		if len(entry) == 0 {
			delete(providers, provider)
		} else {
			providers[provider] = entry
		}
		return nil
	})
}

func (s *CredentialStore) mutate(update func(map[string]interface{}) error) error {
	if s == nil || strings.TrimSpace(s.path) == "" {
		return fmt.Errorf("credential store path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	unlock := lockAuthFile(s.path)
	defer unlock()
	payload := map[string]interface{}{}
	if data, err := os.ReadFile(s.path); err == nil {
		if err := json.Unmarshal(data, &payload); err != nil {
			return fmt.Errorf("decode credential store: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := update(payload); err != nil {
		return err
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(filepath.Dir(s.path), ".auth-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := bytes.NewReader(data).WriteTo(temp); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, s.path)
}

func nestedMap(payload map[string]interface{}, outer, inner string) (map[string]interface{}, bool) {
	parent, _ := payload[outer].(map[string]interface{})
	child, ok := parent[inner].(map[string]interface{})
	return child, ok
}

func ensureMap(payload map[string]interface{}, key string) map[string]interface{} {
	value, _ := payload[key].(map[string]interface{})
	if value == nil {
		value = map[string]interface{}{}
		payload[key] = value
	}
	return value
}

func cloneJSONValue(value interface{}) interface{} {
	data, _ := json.Marshal(value)
	var cloned interface{}
	_ = json.Unmarshal(data, &cloned)
	return cloned
}

func newCredentialStageID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("create credential stage: %w", err)
	}
	return hex.EncodeToString(id[:]), nil
}

type ExternalCredentialResolver struct{}

// Resolve performs best-effort reuse for selected coding CLIs only. OAuth
// refresh stays in modelruntime so protocol adapters can remain transport-only.
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
		if cred := codexCredentialFromFile(path); cred.Token != "" {
			return cred
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
