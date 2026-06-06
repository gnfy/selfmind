package modelruntime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Catalog fetches model IDs through the provider's native or compatible list
// endpoint and caches only non-secret model names on disk.
type Catalog struct {
	cachePath string
}

func NewCatalog(cachePath string) *Catalog {
	return &Catalog{cachePath: expandHome(cachePath)}
}

func DefaultCatalogPath() string {
	return filepath.Join(homeDir(), ".selfmind", "model_cache.json")
}

// Models returns a live model list when possible, with a short local cache and
// profile fallbacks so new provider releases do not require a SelfMind rebuild.
func (c *Catalog) Models(ctx context.Context, profile ProviderProfile, rt Runtime, forceRefresh bool) ([]string, error) {
	cacheKey := profile.ID + "|" + rt.BaseURL + "|" + credentialFingerprint(rt.APIKey)
	if !forceRefresh {
		if cached := c.readCache(cacheKey); len(cached) > 0 {
			return cached, nil
		}
	}
	models, err := c.fetch(ctx, profile, rt)
	if err == nil && len(models) > 0 {
		c.writeCache(cacheKey, models)
		return models, nil
	}
	if len(profile.FallbackModels) > 0 {
		return uniqueSorted(profile.FallbackModels), err
	}
	return nil, err
}

func (c *Catalog) fetch(ctx context.Context, profile ProviderProfile, rt Runtime) ([]string, error) {
	switch profile.ModelList {
	case ModelListAnthropic:
		return fetchAnthropicModels(ctx, rt.BaseURL, rt.APIKey)
	case ModelListGoogle:
		return fetchGoogleModels(ctx, rt.BaseURL, rt.APIKey)
	case ModelListCodex:
		return fetchCodexModels(ctx, rt.APIKey)
	case ModelListOpenAICompatible:
		return fetchOpenAICompatibleModels(ctx, rt.BaseURL, rt.APIKey)
	case ModelListStatic:
		return profile.FallbackModels, nil
	default:
		return fetchOpenAICompatibleModels(ctx, rt.BaseURL, rt.APIKey)
	}
}

func fetchOpenAICompatibleModels(ctx context.Context, baseURL, apiKey string) ([]string, error) {
	return fetchModelIDs(ctx, openAIModelsURL(baseURL), apiKey, nil)
}

func fetchAnthropicModels(ctx context.Context, baseURL, apiKey string) ([]string, error) {
	root := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if root == "" {
		root = "https://api.anthropic.com"
	}
	lower := strings.ToLower(root)
	if strings.HasSuffix(lower, "/v1/messages") {
		root = root[:len(root)-len("/v1/messages")]
	} else if strings.HasSuffix(lower, "/messages") {
		root = root[:len(root)-len("/messages")]
	}
	return fetchModelIDs(ctx, root+"/v1/models", apiKey, map[string]string{"anthropic-version": "2023-06-01"})
}

func fetchGoogleModels(ctx context.Context, baseURL, apiKey string) ([]string, error) {
	root := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if root == "" {
		root = "https://generativelanguage.googleapis.com/v1beta"
	}
	lower := strings.ToLower(root)
	if idx := strings.Index(lower, "/v1beta"); idx >= 0 {
		root = root[:idx+len("/v1beta")]
	}
	modelURL := root + "/models"
	if apiKey != "" {
		modelURL += "?key=" + url.QueryEscape(apiKey)
	}
	return fetchModelIDs(ctx, modelURL, "", nil)
}

func fetchCodexModels(ctx context.Context, apiKey string) ([]string, error) {
	// Codex CLI accounts may expose a richer model list than the public OpenAI
	// API. Fall back to local Codex cache/config before static defaults.
	if apiKey != "" {
		reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "https://chatgpt.com/backend-api/codex/models?client_version=1.0.0", nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err == nil && resp != nil {
			defer resp.Body.Close()
			if resp.StatusCode < 400 {
				var payload interface{}
				if err := json.NewDecoder(resp.Body).Decode(&payload); err == nil {
					if ids := collectModelIDs(payload); len(ids) > 0 {
						return ids, nil
					}
				}
			}
		}
	}
	if ids := codexLocalModels(); len(ids) > 0 {
		return ids, nil
	}
	return []string{"gpt-5.5", "gpt-5.3-codex"}, nil
}

func codexLocalModels() []string {
	codexHome := firstNonEmpty(os.Getenv("CODEX_HOME"), filepath.Join(homeDir(), ".codex"))
	var out []string
	if payload, err := readJSONFile(filepath.Join(codexHome, "models_cache.json")); err == nil {
		out = append(out, collectModelIDs(payload)...)
	}
	if data, err := os.ReadFile(filepath.Join(codexHome, "config.toml")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "model") && strings.Contains(line, "=") {
				parts := strings.SplitN(line, "=", 2)
				model := strings.Trim(strings.TrimSpace(parts[1]), `"'`)
				if model != "" {
					out = append(out, model)
				}
			}
		}
	}
	return uniqueSorted(out)
}

func fetchModelIDs(ctx context.Context, modelURL, apiKey string, headers map[string]string) ([]string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, modelURL, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("x-api-key", apiKey)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("%s %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var payload interface{}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	ids := collectModelIDs(payload)
	if len(ids) == 0 {
		return nil, fmt.Errorf("no models returned")
	}
	return ids, nil
}

func collectModelIDs(value interface{}) []string {
	// Providers are not consistent about response shape. Walk common nested
	// payloads and collect stable identifier fields without binding to one schema.
	var ids []string
	var walk func(interface{})
	walk = func(v interface{}) {
		switch item := v.(type) {
		case map[string]interface{}:
			if id := stringFromInterface(item["id"]); id != "" {
				ids = append(ids, cleanModelID(id))
			}
			if slug := stringFromInterface(item["slug"]); slug != "" {
				ids = append(ids, cleanModelID(slug))
			}
			if name := stringFromInterface(item["name"]); name != "" {
				ids = append(ids, cleanModelID(name))
			}
			for key, child := range item {
				if key == "data" || key == "models" {
					walk(child)
				}
			}
		case []interface{}:
			for _, child := range item {
				walk(child)
			}
		}
	}
	walk(value)
	return uniqueSorted(ids)
}

func (c *Catalog) readCache(key string) []string {
	if c == nil || c.cachePath == "" {
		return nil
	}
	payload, err := readJSONFile(c.cachePath)
	if err != nil {
		return nil
	}
	entry, _ := payload[key].(map[string]interface{})
	if entry == nil {
		return nil
	}
	ts, _ := entry["ts"].(float64)
	if ts <= 0 || time.Since(time.Unix(int64(ts), 0)) > time.Hour {
		return nil
	}
	raw, _ := entry["models"].([]interface{})
	var out []string
	for _, item := range raw {
		if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

func (c *Catalog) writeCache(key string, models []string) {
	if c == nil || c.cachePath == "" || len(models) == 0 {
		return
	}
	payload, _ := readJSONFile(c.cachePath)
	if payload == nil {
		payload = make(map[string]interface{})
	}
	payload[key] = map[string]interface{}{"ts": time.Now().Unix(), "models": models}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(c.cachePath), 0755)
	tmp := c.cachePath + ".tmp"
	// Write via rename so a crash cannot leave the cache as partial JSON.
	if err := os.WriteFile(tmp, data, 0600); err == nil {
		_ = os.Rename(tmp, c.cachePath)
	}
}

func openAIModelsURL(baseURL string) string {
	return normalizeOpenAIRoot(baseURL) + "/models"
}

func normalizeOpenAIRoot(baseURL string) string {
	root := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if root == "" {
		return "https://api.openai.com/v1"
	}
	lower := strings.ToLower(root)
	for _, suffix := range []string{"/chat/completions", "/responses"} {
		if strings.HasSuffix(lower, suffix) {
			root = root[:len(root)-len(suffix)]
			break
		}
	}
	return strings.TrimRight(root, "/")
}

func cleanModelID(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "models/")
	return value
}

func stringFromInterface(value interface{}) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func credentialFingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum[:8])
}
