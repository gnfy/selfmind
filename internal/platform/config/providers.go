package config

import (
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"unicode"

	"github.com/spf13/viper"
	"go.yaml.in/yaml/v3"
)

var providerEndpointFields = map[string]struct{}{
	"api_key": {}, "base_url": {}, "protocol": {}, "model": {}, "context_length": {},
	"extra_headers": {}, "extra_body": {}, "extra_query": {}, "headers": {},
	"max_tokens": {}, "reasoning_effort": {}, "thinking": {}, "service_tier": {}, "quirks": {},
}

var customProviderFields = func() map[string]struct{} {
	out := make(map[string]struct{}, len(providerEndpointFields)+4)
	for key := range providerEndpointFields {
		out[key] = struct{}{}
	}
	out["name"] = struct{}{}
	out["auth"] = struct{}{}
	out["models"] = struct{}{}
	return out
}()

var providerQuirkFields = map[string]struct{}{
	"auth_header": {}, "tool_schema": {}, "system_message_mode": {}, "thinking_mode": {},
	"user_identity_field": {}, "user_agent": {}, "http_version": {}, "prompt_cache": {},
	"responses_store_false": {}, "responses_require_stream": {},
}

var legacyProviderScalarFields = map[string]struct{}{
	"anthropic_api_key": {}, "openai_api_key": {}, "openrouter_api_key": {},
	"gemini_api_key": {}, "minimax_api_key": {},
}

var builtinProviderConfigIDs = map[string]struct{}{
	"openai": {}, "anthropic": {}, "claude-code": {}, "google": {}, "gemini-cli": {},
	"qwen-cli": {}, "codex-cli": {}, "openrouter": {}, "minimax": {}, "minimax-cn": {},
	"minimax-oauth": {}, "kimi-coding": {}, "deepseek": {}, "zai": {}, "alibaba-coding-plan": {},
}

// BuiltinEndpoint returns a user override for one built-in provider. Defaults
// remain owned by modelruntime and therefore do not need YAML materialization.
func (p ProvidersConfig) BuiltinEndpoint(id string) (ProviderEndpoint, bool) {
	id = normalizeProviderConfigID(id)
	switch id {
	case "openai", "openai-api":
		return p.OpenAI, !providerEndpointEmpty(p.OpenAI)
	case "anthropic":
		return p.Anthropic, !providerEndpointEmpty(p.Anthropic)
	case "google", "gemini":
		return p.Google, !providerEndpointEmpty(p.Google)
	}
	for name, endpoint := range p.Builtins {
		if normalizeProviderConfigID(name) == id {
			return endpoint, true
		}
	}
	return ProviderEndpoint{}, false
}

// SetBuiltinEndpoint writes one built-in override without requiring a field in
// ProvidersConfig for every provider registered by modelruntime.
func (p *ProvidersConfig) SetBuiltinEndpoint(id string, endpoint ProviderEndpoint) {
	if p == nil {
		return
	}
	id = normalizeProviderConfigID(id)
	switch id {
	case "openai", "openai-api":
		p.OpenAI = endpoint
		return
	case "anthropic":
		p.Anthropic = endpoint
		return
	case "google", "gemini":
		p.Google = endpoint
		return
	}
	if p.Builtins == nil {
		p.Builtins = make(map[string]ProviderEndpoint)
	}
	if providerEndpointEmpty(endpoint) {
		delete(p.Builtins, id)
		return
	}
	p.Builtins[id] = endpoint
}

func (p ProvidersConfig) CustomProvider(id string) (CustomProvider, bool) {
	id = normalizeProviderConfigID(strings.TrimPrefix(strings.TrimSpace(id), "custom:"))
	for _, provider := range p.Custom {
		if normalizeProviderConfigID(provider.Name) == id {
			return provider, true
		}
	}
	return CustomProvider{}, false
}

func normalizeProviderConfigID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.ReplaceAll(value, "_", "-")
}

func normalizeCustomProtocol(value string) string {
	switch strings.ToLower(strings.TrimSpace(expandEnvRef(value))) {
	case "", "openai", "openai-compatible", "openai_compatible", "chat_completions":
		return "openai-compatible"
	case "anthropic", "anthropic-compatible", "anthropic_messages", "anthropic-messages":
		return "anthropic-compatible"
	case "responses", "responses-compatible", "codex_responses", "codex-responses", "openai_responses":
		return "responses-compatible"
	default:
		return strings.ToLower(strings.TrimSpace(expandEnvRef(value)))
	}
}

func normalizeCustomAuth(value, protocol string) string {
	value = strings.ToLower(strings.TrimSpace(expandEnvRef(value)))
	value = strings.ReplaceAll(value, "_", "-")
	if value != "" {
		return value
	}
	if normalizeCustomProtocol(protocol) == "anthropic-compatible" {
		return "x-api-key"
	}
	return "bearer"
}

func providerEndpointEmpty(endpoint ProviderEndpoint) bool {
	return strings.TrimSpace(endpoint.APIKey) == "" && strings.TrimSpace(endpoint.BaseURL) == "" &&
		strings.TrimSpace(endpoint.Protocol) == "" && strings.TrimSpace(endpoint.Model) == "" &&
		endpoint.ContextLength <= 0 && len(endpoint.Headers) == 0 && len(endpoint.ExtraHeaders) == 0 &&
		len(endpoint.ExtraBody) == 0 && len(endpoint.ExtraQuery) == 0 && endpoint.MaxTokens <= 0 &&
		strings.TrimSpace(endpoint.ReasoningEffort) == "" && len(endpoint.Thinking) == 0 &&
		strings.TrimSpace(endpoint.ServiceTier) == "" && providerQuirksEmpty(endpoint.Quirks)
}

func providerQuirksEmpty(quirks ProviderQuirks) bool {
	return strings.TrimSpace(quirks.AuthHeader) == "" && strings.TrimSpace(quirks.ToolSchema) == "" &&
		strings.TrimSpace(quirks.SystemMessageMode) == "" && strings.TrimSpace(quirks.ThinkingMode) == "" &&
		strings.TrimSpace(quirks.UserIdentityField) == "" && strings.TrimSpace(quirks.UserAgent) == "" &&
		strings.TrimSpace(quirks.HTTPVersion) == "" && quirks.ResponsesStoreFalse == nil &&
		quirks.ResponsesRequireStream == nil && quirks.PromptCache == nil
}

// MarshalYAML keeps providers.custom map-shaped while retaining the in-memory
// slice during the compatibility window. It also omits built-in defaults that
// Normalize materializes for legacy callers.
func (p ProvidersConfig) MarshalYAML() (interface{}, error) {
	out := make(map[string]interface{})
	appendBuiltin := func(id string, endpoint ProviderEndpoint) {
		endpoint = stripMaterializedProviderDefaults(id, endpoint)
		if !providerEndpointEmpty(endpoint) {
			out[id] = endpoint
		}
	}
	appendBuiltin("openai", p.OpenAI)
	appendBuiltin("anthropic", p.Anthropic)
	appendBuiltin("google", p.Google)
	for id, endpoint := range p.Builtins {
		id = normalizeProviderConfigID(id)
		if id == "" || id == "custom" {
			continue
		}
		appendBuiltin(id, endpoint)
	}
	if len(p.Custom) > 0 {
		custom := make(map[string]customProviderWire, len(p.Custom))
		for _, provider := range p.Custom {
			id := normalizeProviderConfigID(provider.Name)
			if id == "" {
				continue
			}
			wire := customProviderWire(provider)
			wire.Name = ""
			custom[id] = wire
		}
		if len(custom) > 0 {
			out["custom"] = custom
		}
	}
	return out, nil
}

type customProviderWire CustomProvider

func stripMaterializedProviderDefaults(id string, endpoint ProviderEndpoint) ProviderEndpoint {
	defaults := map[string]struct{ baseURL, protocol string }{
		"openai":    {"https://api.openai.com/v1", "openai_chat"},
		"anthropic": {"https://api.anthropic.com", "anthropic_messages"},
		"google":    {"https://generativelanguage.googleapis.com/v1beta/openai/chat/completions", "openai_compatible"},
	}
	if value, ok := defaults[id]; ok {
		if strings.EqualFold(strings.TrimRight(endpoint.BaseURL, "/"), strings.TrimRight(value.baseURL, "/")) {
			endpoint.BaseURL = ""
		}
		if strings.EqualFold(strings.TrimSpace(endpoint.Protocol), value.protocol) {
			endpoint.Protocol = ""
		}
	}
	return endpoint
}

// prepareProviderConfig converts the new map-shaped custom block into the
// legacy in-memory slice before Viper's mapstructure decode. SaveConfig writes
// only the map shape through ProvidersConfig.MarshalYAML.
func prepareProviderConfig(v *viper.Viper) error {
	if v == nil {
		return nil
	}
	raw := v.Get("providers.custom")
	providers, ok := stringInterfaceMap(raw)
	if !ok {
		return nil
	}
	ids := make([]string, 0, len(providers))
	for id := range providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	list := make([]interface{}, 0, len(ids))
	for _, id := range ids {
		entry, ok := stringInterfaceMap(providers[id])
		if !ok {
			return fmt.Errorf("providers.custom.%s must be a mapping", id)
		}
		copyEntry := make(map[string]interface{}, len(entry)+1)
		for key, value := range entry {
			copyEntry[key] = value
		}
		copyEntry["name"] = id
		list = append(list, copyEntry)
	}
	v.Set("providers.custom", list)
	return nil
}

func stringInterfaceMap(value interface{}) (map[string]interface{}, bool) {
	switch typed := value.(type) {
	case map[string]interface{}:
		return typed, true
	case map[interface{}]interface{}:
		out := make(map[string]interface{}, len(typed))
		for key, item := range typed {
			text, ok := key.(string)
			if !ok {
				return nil, false
			}
			out[text] = item
		}
		return out, true
	default:
		return nil, false
	}
}

func validateProviderYAML(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	root := yamlMappingRoot(&doc)
	providers := yamlMappingValue(root, "providers")
	if providers == nil {
		return nil
	}
	if providers.Kind != yaml.MappingNode {
		return fmt.Errorf("providers must be a mapping")
	}
	seen := make(map[string]string)
	for i := 0; i+1 < len(providers.Content); i += 2 {
		key, value := providers.Content[i].Value, providers.Content[i+1]
		normalizedKey := normalizeProviderConfigID(key)
		if previous := seen[normalizedKey]; previous != "" {
			return fmt.Errorf("providers.%s duplicates providers.%s after normalization", key, previous)
		}
		seen[normalizedKey] = key
		if key == "custom" {
			if err := validateCustomProviderYAML(value); err != nil {
				return err
			}
			continue
		}
		if _, legacy := legacyProviderScalarFields[key]; legacy {
			continue
		}
		if _, builtin := builtinProviderConfigIDs[normalizeProviderConfigID(key)]; !builtin {
			suggestion := closestProviderField(key, builtinProviderConfigIDs)
			if suggestion != "" {
				return fmt.Errorf("providers.%s is unknown; did you mean providers.%s? Put user-defined connections under providers.custom", key, suggestion)
			}
			return fmt.Errorf("providers.%s is unknown; put user-defined connections under providers.custom", key)
		}
		if err := validateProviderEndpointNode("providers."+key, value, providerEndpointFields); err != nil {
			return err
		}
	}
	return nil
}

func validateCustomProviderYAML(node *yaml.Node) error {
	seen := make(map[string]string)
	validateID := func(raw, path string) error {
		id := normalizeProviderConfigID(raw)
		if id == "" {
			return fmt.Errorf("%s has an empty provider id", path)
		}
		if _, builtin := builtinProviderConfigIDs[id]; builtin {
			return fmt.Errorf("%s id %q collides with a built-in provider", path, raw)
		}
		if previous := seen[id]; previous != "" {
			return fmt.Errorf("%s id %q duplicates %q after normalization", path, raw, previous)
		}
		seen[id] = raw
		return nil
	}
	switch node.Kind {
	case yaml.SequenceNode: // legacy list
		for index, entry := range node.Content {
			path := fmt.Sprintf("providers.custom[%d]", index)
			if err := validateProviderEndpointNode(path, entry, customProviderFields); err != nil {
				return err
			}
			name := yamlMappingValue(entry, "name")
			if name == nil || name.Kind != yaml.ScalarNode {
				return fmt.Errorf("%s.name is required", path)
			}
			if err := validateID(name.Value, path); err != nil {
				return err
			}
		}
		return nil
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			id, entry := node.Content[i].Value, node.Content[i+1]
			if err := validateID(id, "providers.custom."+id); err != nil {
				return err
			}
			if err := validateProviderEndpointNode("providers.custom."+id, entry, customProviderFields); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("providers.custom must be a mapping")
	}
}

func validateProviderEndpointNode(path string, node *yaml.Node, allowed map[string]struct{}) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("%s must be a mapping", path)
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i].Value, node.Content[i+1]
		if _, ok := allowed[key]; !ok {
			suggestion := closestProviderField(key, allowed)
			if suggestion != "" {
				return fmt.Errorf("%s.%s is unknown; did you mean %s?", path, key, suggestion)
			}
			return fmt.Errorf("%s.%s is unknown", path, key)
		}
		switch key {
		case "extra_headers", "headers":
			if err := validateHeaderNode(path+"."+key, value); err != nil {
				return err
			}
		case "extra_body", "extra_query":
			if err := validateProviderExtraNode(path+"."+key, value); err != nil {
				return err
			}
		case "quirks":
			if err := validateProviderEndpointNode(path+".quirks", value, providerQuirkFields); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateProviderExtraNode(path string, node *yaml.Node) error {
	if node == nil {
		return nil
	}
	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := strings.TrimSpace(node.Content[i].Value), node.Content[i+1]
			if credentialExtraKey(key) {
				return fmt.Errorf("%s.%s is credential-bearing; use the provider credential store", path, key)
			}
			if err := validateProviderExtraNode(path+"."+key, value); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for index, value := range node.Content {
			if err := validateProviderExtraNode(fmt.Sprintf("%s[%d]", path, index), value); err != nil {
				return err
			}
		}
	}
	return nil
}

func credentialExtraKey(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.NewReplacer("-", "_", " ", "_").Replace(value)
	switch value {
	case "api_key", "apikey", "access_token", "auth_token", "authorization", "credential", "credentials", "password", "secret", "token":
		return true
	default:
		return false
	}
}

func validateHeaderNode(path string, node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("%s must be a mapping", path)
	}
	seen := make(map[string]string)
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := strings.TrimSpace(node.Content[i].Value), node.Content[i+1]
		lower := strings.ToLower(key)
		if previous := seen[lower]; previous != "" {
			return fmt.Errorf("%s contains duplicate header %q (also written as %q)", path, key, previous)
		}
		seen[lower] = key
		if !validHeaderName(key) {
			return fmt.Errorf("%s contains invalid header name %q", path, key)
		}
		if value.Kind != yaml.ScalarNode || strings.ContainsAny(value.Value, "\r\n") {
			return fmt.Errorf("%s.%s contains an invalid header value", path, key)
		}
		switch lower {
		case "host", "content-length", "transfer-encoding", "connection", "proxy-connection":
			return fmt.Errorf("%s.%s is managed by the HTTP transport", path, key)
		case "authorization", "proxy-authorization", "x-api-key", "api-key":
			return fmt.Errorf("%s.%s is credential-bearing; use the provider credential store", path, key)
		}
	}
	return nil
}

func validHeaderName(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("!#$%&'*+-.^_`|~", r) {
			continue
		}
		return false
	}
	return true
}

func yamlMappingRoot(doc *yaml.Node) *yaml.Node {
	if doc == nil || len(doc.Content) == 0 {
		return nil
	}
	node := doc.Content[0]
	if node.Kind == yaml.MappingNode {
		return node
	}
	return nil
}

func yamlMappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func closestProviderField(value string, allowed map[string]struct{}) string {
	value = strings.ToLower(strings.TrimSpace(value))
	best, distance := "", 4
	for candidate := range allowed {
		if d := editDistance(value, candidate); d < distance {
			best, distance = candidate, d
		}
	}
	return best
}

func editDistance(a, b string) int {
	previous := make([]int, len(b)+1)
	for i := range previous {
		previous[i] = i
	}
	for i, ar := range a {
		current := make([]int, len(b)+1)
		current[0] = i + 1
		for j, br := range b {
			cost := 0
			if ar != br {
				cost = 1
			}
			current[j+1] = minInt(current[j]+1, previous[j+1]+1, previous[j]+cost)
		}
		previous = current
	}
	return previous[len(b)]
}

func minInt(values ...int) int {
	result := values[0]
	for _, value := range values[1:] {
		if value < result {
			result = value
		}
	}
	return result
}

func validateCustomProvider(provider CustomProvider) error {
	id := normalizeProviderConfigID(provider.Name)
	if id == "" {
		return fmt.Errorf("providers.custom contains an empty provider id")
	}
	if _, builtin := builtinProviderConfigIDs[id]; builtin {
		return fmt.Errorf("providers.custom.%s collides with a built-in provider", id)
	}
	protocol := strings.ToLower(strings.TrimSpace(provider.Protocol))
	switch protocol {
	case "", "openai", "openai-compatible", "openai_compatible", "chat_completions",
		"anthropic", "anthropic-compatible", "anthropic_messages", "anthropic-messages",
		"responses", "responses-compatible", "codex_responses", "codex-responses", "openai_responses":
	default:
		return fmt.Errorf("providers.custom.%s.protocol %q is unsupported; use openai-compatible, anthropic-compatible, or responses-compatible", id, provider.Protocol)
	}
	auth := strings.ToLower(strings.TrimSpace(provider.Auth))
	switch auth {
	case "", "bearer", "x-api-key", "x_api_key", "none":
	default:
		return fmt.Errorf("providers.custom.%s.auth %q is unsupported; use bearer, x-api-key, or none", id, provider.Auth)
	}
	if raw := strings.TrimSpace(provider.BaseURL); raw != "" {
		parsed, err := url.Parse(raw)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("providers.custom.%s.base_url must be an http(s) URL", id)
		}
		if parsed.User != nil {
			return fmt.Errorf("providers.custom.%s.base_url must not contain credentials", id)
		}
		if parsed.Fragment != "" {
			return fmt.Errorf("providers.custom.%s.base_url must not contain a fragment", id)
		}
	}
	return nil
}
