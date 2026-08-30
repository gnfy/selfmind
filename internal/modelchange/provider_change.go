package modelchange

import (
	"fmt"
	"net/url"
	"reflect"
	"strings"

	"selfmind/internal/modelruntime"
	"selfmind/internal/platform/config"
)

// ProviderConnection is the non-secret, provider-owned connection surface
// that may participate in a model transaction. Model IDs remain route-owned;
// API keys remain in CredentialStore.
type ProviderConnection struct {
	ID              string                 `json:"id"`
	Custom          bool                   `json:"custom,omitempty"`
	BaseURL         string                 `json:"base_url,omitempty"`
	Protocol        string                 `json:"protocol,omitempty"`
	Auth            string                 `json:"auth,omitempty"`
	ExtraHeaders    map[string]string      `json:"extra_headers,omitempty"`
	ExtraBody       map[string]interface{} `json:"extra_body,omitempty"`
	ExtraQuery      map[string]interface{} `json:"extra_query,omitempty"`
	ContextLength   int                    `json:"context_length,omitempty"`
	MaxTokens       int                    `json:"max_tokens,omitempty"`
	ReasoningEffort string                 `json:"reasoning_effort,omitempty"`
	Thinking        map[string]interface{} `json:"thinking,omitempty"`
	ServiceTier     string                 `json:"service_tier,omitempty"`
	Quirks          config.ProviderQuirks  `json:"quirks,omitempty"`
}

type ProviderPatch struct {
	Connection       ProviderConnection `json:"connection"`
	Delete           bool               `json:"delete,omitempty"`
	PreserveAdvanced bool               `json:"preserve_advanced,omitempty"`
}

type ProviderChange struct {
	ID              string             `json:"id"`
	Custom          bool               `json:"custom,omitempty"`
	PreviousExists  bool               `json:"previous_exists"`
	Previous        ProviderConnection `json:"previous,omitempty"`
	CandidateExists bool               `json:"candidate_exists"`
	Candidate       ProviderConnection `json:"candidate,omitempty"`
}

func BuildProviderChanges(cfg *config.Config, patches []ProviderPatch) ([]ProviderChange, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config is required")
	}
	changes := make([]ProviderChange, 0, len(patches))
	seen := make(map[string]struct{})
	for _, patch := range patches {
		candidate := normalizeProviderConnection(patch.Connection)
		if candidate.ID == "" {
			return nil, fmt.Errorf("provider connection id is required")
		}
		key := fmt.Sprintf("%t:%s", candidate.Custom, candidate.ID)
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("provider connection %q is patched more than once", candidate.ID)
		}
		seen[key] = struct{}{}
		if !candidate.Custom {
			if _, current := cfg.Providers.BuiltinEndpoint(candidate.ID); !current {
				if _, legacy := cfg.ProviderProfiles[candidate.ID]; legacy {
					return nil, fmt.Errorf("provider %q still uses legacy provider_profiles; run `selfmind config upgrade` before editing this connection", candidate.ID)
				}
			}
		}
		previous, exists := providerConnectionFromConfig(cfg, candidate.ID, candidate.Custom)
		if patch.PreserveAdvanced && exists && !patch.Delete {
			preserved := cloneProviderConnection(previous)
			preserved.BaseURL = candidate.BaseURL
			if candidate.Custom {
				preserved.Protocol = candidate.Protocol
				preserved.Auth = candidate.Auth
			}
			candidate = normalizeProviderConnection(preserved)
		}
		if patch.Delete && !candidate.Custom {
			return nil, fmt.Errorf("built-in provider %q cannot be deleted; save an empty override to use its defaults", candidate.ID)
		}
		change := ProviderChange{
			ID: candidate.ID, Custom: candidate.Custom,
			PreviousExists: exists, Previous: previous,
			CandidateExists: !patch.Delete, Candidate: candidate,
		}
		if patch.Delete && !exists {
			continue
		}
		if !patch.Delete {
			if err := validateProviderConnection(candidate); err != nil {
				return nil, err
			}
			if exists && providerConnectionsEqual(previous, candidate) {
				continue
			}
		}
		changes = append(changes, change)
	}
	return changes, nil
}

func ApplyProviderChanges(cfg *config.Config, changes []ProviderChange, candidate bool) {
	if cfg == nil {
		return
	}
	for _, change := range changes {
		connection, exists := change.Previous, change.PreviousExists
		if candidate {
			connection, exists = change.Candidate, change.CandidateExists
		}
		applyProviderConnection(cfg, change.ID, change.Custom, connection, exists)
	}
}

func ProviderChangesMatch(cfg *config.Config, changes []ProviderChange, candidate bool) bool {
	for _, change := range changes {
		want, wantExists := change.Previous, change.PreviousExists
		if candidate {
			want, wantExists = change.Candidate, change.CandidateExists
		}
		got, gotExists := providerConnectionFromConfig(cfg, change.ID, change.Custom)
		if gotExists != wantExists || (wantExists && !providerConnectionsEqual(got, want)) {
			return false
		}
	}
	return true
}

func providerConnectionFromConfig(cfg *config.Config, id string, custom bool) (ProviderConnection, bool) {
	id = normalizeProviderConnectionID(id)
	if cfg == nil || id == "" {
		return ProviderConnection{}, false
	}
	if custom {
		provider, ok := cfg.Providers.CustomProvider(id)
		if !ok {
			return ProviderConnection{}, false
		}
		return normalizeProviderConnection(ProviderConnection{
			ID: id, Custom: true, BaseURL: provider.BaseURL, Protocol: provider.Protocol, Auth: provider.Auth,
			ExtraHeaders: provider.ExtraHeaders, ExtraBody: provider.ExtraBody, ExtraQuery: provider.ExtraQuery,
			ContextLength: provider.ContextLength, MaxTokens: provider.MaxTokens,
			ReasoningEffort: provider.ReasoningEffort, Thinking: provider.Thinking,
			ServiceTier: provider.ServiceTier, Quirks: provider.Quirks,
		}), true
	}
	endpoint, ok := cfg.Providers.BuiltinEndpoint(id)
	if !ok {
		return ProviderConnection{}, false
	}
	return normalizeProviderConnection(ProviderConnection{
		ID: id, BaseURL: endpoint.BaseURL, Protocol: endpoint.Protocol,
		ExtraHeaders: endpoint.ExtraHeaders, ExtraBody: endpoint.ExtraBody, ExtraQuery: endpoint.ExtraQuery,
		ContextLength: endpoint.ContextLength, MaxTokens: endpoint.MaxTokens,
		ReasoningEffort: endpoint.ReasoningEffort, Thinking: endpoint.Thinking,
		ServiceTier: endpoint.ServiceTier, Quirks: endpoint.Quirks,
	}), true
}

func applyProviderConnection(cfg *config.Config, id string, custom bool, connection ProviderConnection, exists bool) {
	id = normalizeProviderConnectionID(id)
	if custom {
		providers := cfg.Providers.Custom[:0]
		for _, provider := range cfg.Providers.Custom {
			if normalizeProviderConnectionID(provider.Name) != id {
				providers = append(providers, provider)
			}
		}
		cfg.Providers.Custom = providers
		if exists {
			cfg.Providers.Custom = append(cfg.Providers.Custom, config.CustomProvider{
				Name: id, BaseURL: connection.BaseURL, Protocol: connection.Protocol, Auth: connection.Auth,
				ExtraHeaders: cloneStringMap(connection.ExtraHeaders), ExtraBody: cloneAnyMap(connection.ExtraBody),
				ExtraQuery: cloneAnyMap(connection.ExtraQuery), ContextLength: connection.ContextLength,
				MaxTokens: connection.MaxTokens, ReasoningEffort: connection.ReasoningEffort,
				Thinking: cloneAnyMap(connection.Thinking), ServiceTier: connection.ServiceTier, Quirks: connection.Quirks,
			})
		}
		return
	}
	if !exists {
		cfg.Providers.SetBuiltinEndpoint(id, config.ProviderEndpoint{})
		return
	}
	cfg.Providers.SetBuiltinEndpoint(id, config.ProviderEndpoint{
		BaseURL: connection.BaseURL, Protocol: connection.Protocol,
		ExtraHeaders: cloneStringMap(connection.ExtraHeaders), ExtraBody: cloneAnyMap(connection.ExtraBody),
		ExtraQuery: cloneAnyMap(connection.ExtraQuery), ContextLength: connection.ContextLength,
		MaxTokens: connection.MaxTokens, ReasoningEffort: connection.ReasoningEffort,
		Thinking: cloneAnyMap(connection.Thinking), ServiceTier: connection.ServiceTier, Quirks: connection.Quirks,
	})
}

func validateProviderConnection(connection ProviderConnection) error {
	if raw := strings.TrimSpace(connection.BaseURL); raw != "" {
		parsed, err := url.Parse(raw)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("provider %q base_url must be an http(s) URL", connection.ID)
		}
		if parsed.User != nil || parsed.Fragment != "" {
			return fmt.Errorf("provider %q base_url must not contain credentials or a fragment", connection.ID)
		}
	}
	seenHeaders := make(map[string]string)
	for name, value := range connection.ExtraHeaders {
		lower := strings.ToLower(strings.TrimSpace(name))
		if previous := seenHeaders[lower]; previous != "" {
			return fmt.Errorf("provider %q contains duplicate header %q (also written as %q)", connection.ID, name, previous)
		}
		seenHeaders[lower] = name
		if strings.ContainsAny(name+value, "\r\n") {
			return fmt.Errorf("provider %q contains an invalid header", connection.ID)
		}
		switch lower {
		case "host", "content-length", "transfer-encoding", "connection", "proxy-connection":
			return fmt.Errorf("provider %q header %q is managed by the HTTP transport", connection.ID, name)
		case "authorization", "proxy-authorization", "x-api-key", "api-key":
			return fmt.Errorf("provider %q header %q is credential-bearing; use the provider credential store", connection.ID, name)
		}
	}
	if path, found := credentialExtraPath(connection.ExtraBody, "extra_body"); found {
		return fmt.Errorf("provider %q %s is credential-bearing; use the provider credential store", connection.ID, path)
	}
	if path, found := credentialExtraPath(connection.ExtraQuery, "extra_query"); found {
		return fmt.Errorf("provider %q %s is credential-bearing; use the provider credential store", connection.ID, path)
	}
	if connection.Custom {
		if _, builtin := modelruntime.NewRegistry().Resolve(connection.ID); builtin {
			return fmt.Errorf("custom provider id %q collides with a built-in provider", connection.ID)
		}
		if strings.TrimSpace(connection.BaseURL) == "" {
			return fmt.Errorf("custom provider %q requires base_url", connection.ID)
		}
		switch connection.Protocol {
		case "openai-compatible", "anthropic-compatible", "responses-compatible":
		default:
			return fmt.Errorf("custom provider %q protocol must be openai-compatible, anthropic-compatible, or responses-compatible", connection.ID)
		}
		switch connection.Auth {
		case "bearer", "x-api-key", "none":
		default:
			return fmt.Errorf("custom provider %q auth must be bearer, x-api-key, or none", connection.ID)
		}
	}
	return nil
}

func credentialExtraPath(values map[string]interface{}, prefix string) (string, bool) {
	for key, value := range values {
		normalized := strings.ToLower(strings.TrimSpace(key))
		normalized = strings.NewReplacer("-", "_", " ", "_").Replace(normalized)
		switch normalized {
		case "api_key", "apikey", "access_token", "auth_token", "authorization", "credential", "credentials", "password", "secret", "token":
			return prefix + "." + key, true
		}
		switch typed := value.(type) {
		case map[string]interface{}:
			if path, found := credentialExtraPath(typed, prefix+"."+key); found {
				return path, true
			}
		case []interface{}:
			for index, item := range typed {
				if nested, ok := item.(map[string]interface{}); ok {
					if path, found := credentialExtraPath(nested, fmt.Sprintf("%s.%s[%d]", prefix, key, index)); found {
						return path, true
					}
				}
			}
		}
	}
	return "", false
}

func normalizeProviderConnection(connection ProviderConnection) ProviderConnection {
	connection.ID = normalizeProviderConnectionID(connection.ID)
	connection.BaseURL = strings.TrimRight(strings.TrimSpace(connection.BaseURL), "/")
	connection.Protocol = strings.ToLower(strings.TrimSpace(connection.Protocol))
	if connection.Custom {
		connection.Protocol = strings.ReplaceAll(connection.Protocol, "_", "-")
		switch connection.Protocol {
		case "", "openai", "openai-compatible", "chat-completions":
			connection.Protocol = "openai-compatible"
		case "anthropic", "anthropic-messages", "anthropic-compatible":
			connection.Protocol = "anthropic-compatible"
		case "responses", "codex-responses", "openai-responses", "responses-compatible":
			connection.Protocol = "responses-compatible"
		}
		connection.Auth = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(connection.Auth), "_", "-"))
		if connection.Auth == "" {
			if connection.Protocol == "anthropic-compatible" {
				connection.Auth = "x-api-key"
			} else {
				connection.Auth = "bearer"
			}
		}
	}
	return connection
}

func normalizeProviderConnectionID(value string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(strings.TrimPrefix(value, "custom:"))), "_", "-")
}

func providerConnectionsEqual(left, right ProviderConnection) bool {
	left = normalizeProviderConnection(left)
	right = normalizeProviderConnection(right)
	return reflect.DeepEqual(left, right)
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	out := make(map[string]string, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

func cloneAnyMap(source map[string]interface{}) map[string]interface{} {
	if source == nil {
		return nil
	}
	out := make(map[string]interface{}, len(source))
	for key, value := range source {
		out[key] = cloneAnyValue(value)
	}
	return out
}

func cloneAnyValue(value interface{}) interface{} {
	switch typed := value.(type) {
	case map[string]interface{}:
		return cloneAnyMap(typed)
	case []interface{}:
		out := make([]interface{}, len(typed))
		for i, item := range typed {
			out[i] = cloneAnyValue(item)
		}
		return out
	default:
		return value
	}
}
