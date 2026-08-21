package cliapp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	appcore "selfmind/internal/app"
	"selfmind/internal/modelruntime"
	"selfmind/internal/platform/config"
	"selfmind/internal/tools"
)

type modelChoice struct {
	ID          string
	Label       string
	Kind        string
	CustomIndex int
}

func (a *App) runModelCommandIfRequested() (bool, int) {
	if len(a.args) < 2 || a.args[1] != "model" {
		return false, 0
	}

	cfg, err := config.LoadConfig(config.Options{Path: a.configPath, CreateIfMissing: true})
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return true, 1
	}

	args := a.args[2:]
	if len(args) == 0 {
		return true, a.runInteractiveModelPicker(cfg)
	}

	switch args[0] {
	case "current":
		a.printCurrentModel(cfg)
		return true, 0
	case "check":
		options, err := parseModelCheckOptions(args[1:])
		if err != nil {
			fmt.Fprintln(a.stderr, err)
			return true, 2
		}
		return true, a.checkCurrentModel(cfg, options)
	case "list":
		a.printConfiguredProviders(cfg)
		return true, 0
	case "set":
		return true, a.setModelFromArgs(cfg, args[1:])
	default:
		fmt.Fprintln(a.stderr, "usage: selfmind model [current|check [--live] [--role <name>]|list|set <provider> <model> [--reasoning <level|auto>] [--service-tier <tier|auto>]]")
		return true, 2
	}
}

func (a *App) runInteractiveModelPicker(cfg *config.Config) int {
	fmt.Fprintln(a.stdout, "SelfMind model setup")
	a.printCurrentModel(cfg)
	fmt.Fprintln(a.stdout)

	choices := a.modelChoices(cfg)
	index, err := a.promptChoice("Choose a provider:", choiceLabels(choices))
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	if index < 0 || index >= len(choices) {
		return 1
	}

	choice := choices[index]
	switch choice.Kind {
	case "builtin":
		return a.configureBuiltinProvider(cfg, choice.ID)
	case "custom_saved":
		return a.configureSavedCustomProvider(cfg, choice.CustomIndex)
	case "custom_new":
		return a.configureCustomEndpoint(cfg)
	case "remove_custom":
		return a.removeCustomProvider(cfg)
	case "skip":
		return 0
	default:
		fmt.Fprintf(a.stderr, "unknown provider choice: %s\n", choice.ID)
		return 1
	}
}

func (a *App) modelChoices(cfg *config.Config) []modelChoice {
	// Keep the interactive order close to Hermes: first major providers, then
	// manual custom endpoints, then auth-reuse/coding-plan profiles.
	choices := []modelChoice{
		{ID: "openai", Label: "OpenAI", Kind: "builtin"},
		{ID: "anthropic", Label: "Anthropic", Kind: "builtin"},
		{ID: "google", Label: "Google", Kind: "builtin"},
		{ID: "custom", Label: "Custom endpoint (enter URL manually)", Kind: "custom_new"},
		{ID: "codex-cli", Label: "Codex CLI (reuse login)", Kind: "builtin"},
		{ID: "claude-code", Label: "Claude Code (reuse login)", Kind: "builtin"},
		{ID: "gemini-cli", Label: "Gemini CLI (reuse login)", Kind: "builtin"},
		{ID: "qwen-cli", Label: "Qwen CLI (reuse login)", Kind: "builtin"},
		{ID: "minimax", Label: "MiniMax", Kind: "builtin"},
		{ID: "minimax-oauth", Label: "MiniMax OAuth", Kind: "builtin"},
		{ID: "kimi-coding", Label: "Kimi Coding Plan", Kind: "builtin"},
		{ID: "openrouter", Label: "OpenRouter", Kind: "builtin"},
		{ID: "deepseek", Label: "DeepSeek", Kind: "builtin"},
		{ID: "zai", Label: "Z.AI / GLM", Kind: "builtin"},
		{ID: "alibaba-coding-plan", Label: "Alibaba Coding Plan", Kind: "builtin"},
	}
	for i, cp := range cfg.Providers.Custom {
		name := strings.TrimSpace(cp.Name)
		if name == "" {
			continue
		}
		choices = append(choices, modelChoice{
			ID:          "custom:" + name,
			Label:       fmt.Sprintf("%s (custom)", name),
			Kind:        "custom_saved",
			CustomIndex: i,
		})
	}
	if len(cfg.Providers.Custom) > 0 {
		choices = append(choices, modelChoice{ID: "remove_custom", Label: "Remove a custom endpoint", Kind: "remove_custom"})
	}
	choices = append(choices, modelChoice{ID: "skip", Label: "Cancel", Kind: "skip"})
	return choices
}

func (a *App) configureBuiltinProvider(cfg *config.Config, provider string) int {
	resolver := modelruntime.NewResolver(cfg)
	profile, ok := resolver.Registry().Resolve(provider)
	if !ok {
		fmt.Fprintf(a.stderr, "unknown provider: %s\n", provider)
		return 1
	}

	endpoint := providerEndpointForModelCommand(cfg, profile.ID)
	endpoint.BaseURL = firstNonEmpty(endpoint.BaseURL, profile.BaseURL)
	endpoint.Protocol = modelruntime.NormalizeProtocol(firstNonEmpty(endpoint.Protocol, profile.Protocol))

	key := endpoint.APIKey
	if profile.AuthType == modelruntime.AuthExternalOAuth || profile.AuthType == modelruntime.AuthMiniMaxOAuth {
		// Reused CLI logins stay owned by their source tool, so SelfMind stores
		// the model/base URL choice but not the discovered OAuth token.
		rt, err := resolver.Resolve(a.ctx, modelruntime.Selection{
			Provider: profile.ID,
			Model:    firstNonEmpty(endpoint.Model, cfg.EffectiveModel()),
			BaseURL:  endpoint.BaseURL,
		})
		if err == nil {
			key = rt.APIKey
			endpoint.BaseURL = firstNonEmpty(endpoint.BaseURL, rt.BaseURL)
			endpoint.Model = firstNonEmpty(endpoint.Model, rt.Model)
			endpoint.Protocol = modelruntime.NormalizeProtocol(firstNonEmpty(endpoint.Protocol, rt.Protocol))
			fmt.Fprintf(a.stdout, "Using %s credentials from %s\n", profile.DisplayName, rt.CredentialSource)
		} else {
			fmt.Fprintf(a.stdout, "Could not find reusable %s login: %v\n", profile.DisplayName, err)
			fmt.Fprintln(a.stdout, externalLoginHint(profile.ID))
		}
	} else {
		var err error
		key, err = a.promptAPIKey(profile.DisplayName, endpoint.APIKey)
		if err != nil {
			fmt.Fprintln(a.stderr, err)
			return 1
		}
		endpoint.APIKey = key
	}

	rt := modelruntime.Runtime{
		Provider: profile.ID,
		Model:    firstNonEmpty(endpoint.Model, cfg.EffectiveModel(), firstModelChoice(profile.FallbackModels)),
		Protocol: endpoint.Protocol,
		BaseURL:  endpoint.BaseURL,
		APIKey:   key,
		AuthType: profile.AuthType,
	}
	models, err := modelruntime.NewCatalog(modelruntime.DefaultCatalogPath()).Models(a.ctx, profile, rt, false)
	if err != nil {
		fmt.Fprintf(a.stdout, "Could not load remote model list: %v\n", err)
	}
	model, err := a.promptModel(models, firstNonEmpty(endpoint.Model, cfg.EffectiveModel(), firstModelChoice(profile.FallbackModels)))
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	endpoint.Model = ""
	if profile.AuthType == modelruntime.AuthExternalOAuth || profile.AuthType == modelruntime.AuthMiniMaxOAuth {
		endpoint.APIKey = ""
	}

	setProviderEndpointForModelCommand(cfg, profile.ID, endpoint)
	cfg.SetPrimaryModel(profile.ID, model, "")
	if err := config.SaveConfig(cfg.Path, cfg); err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	fmt.Fprintf(a.stdout, "Saved model: %s / %s\nConfig: %s\n", profile.ID, model, cfg.Path)
	return 0
}

func (a *App) configureSavedCustomProvider(cfg *config.Config, index int) int {
	if index < 0 || index >= len(cfg.Providers.Custom) {
		fmt.Fprintln(a.stderr, "custom provider not found")
		return 1
	}
	cp := cfg.Providers.Custom[index]
	key, err := a.promptAPIKey(cp.Name, cp.APIKey)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	cp.APIKey = key

	models, err := fetchOpenAICompatibleModels(a.ctx, cp.BaseURL, key)
	if err != nil {
		fmt.Fprintf(a.stdout, "Could not load remote model list: %v\n", err)
	}
	model, err := a.promptModel(models, firstNonEmpty(cp.Model, cfg.EffectiveModel(), ""))
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	cp.Model = model
	if cp.Protocol == "" {
		cp.Protocol = "openai_compatible"
	}
	cfg.Providers.Custom[index] = cp
	cfg.SetPrimaryModel("custom:"+cp.Name, model, "")
	if err := config.SaveConfig(cfg.Path, cfg); err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	fmt.Fprintf(a.stdout, "Saved model: custom:%s / %s\nConfig: %s\n", cp.Name, model, cfg.Path)
	return 0
}

func (a *App) configureCustomEndpoint(cfg *config.Config) int {
	baseURL, err := a.promptInput("Base URL", "")
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		fmt.Fprintln(a.stderr, "base URL is required")
		return 2
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		fmt.Fprintln(a.stderr, "base URL must be an http(s) URL")
		return 2
	}
	if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		fmt.Fprintln(a.stderr, "base URL must be an http(s) URL")
		return 2
	}

	nameDefault := customNameFromURL(baseURL)
	name, err := a.promptInput("Name", nameDefault)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	name = uniqueCustomName(cfg, firstNonEmpty(name, nameDefault), baseURL)

	key, err := a.promptAPIKey(name, "")
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}

	models, err := fetchOpenAICompatibleModels(a.ctx, baseURL, key)
	if err != nil {
		fmt.Fprintf(a.stdout, "Could not load remote model list: %v\n", err)
	}
	model, err := a.promptModel(models, "")
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}

	cp := config.CustomProvider{
		Name:     name,
		BaseURL:  normalizeOpenAIRoot(baseURL),
		APIKey:   key,
		Protocol: "openai_compatible",
		Model:    model,
	}
	upsertCustomProvider(cfg, cp)
	cfg.SetPrimaryModel("custom:"+name, model, "")
	if err := config.SaveConfig(cfg.Path, cfg); err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	fmt.Fprintf(a.stdout, "Saved model: custom:%s / %s\nConfig: %s\n", name, model, cfg.Path)
	return 0
}

func (a *App) removeCustomProvider(cfg *config.Config) int {
	choices := make([]string, 0, len(cfg.Providers.Custom))
	indexes := make([]int, 0, len(cfg.Providers.Custom))
	for i, cp := range cfg.Providers.Custom {
		if strings.TrimSpace(cp.Name) == "" {
			continue
		}
		choices = append(choices, fmt.Sprintf("%s (%s)", cp.Name, cp.BaseURL))
		indexes = append(indexes, i)
	}
	if len(choices) == 0 {
		fmt.Fprintln(a.stdout, "No custom endpoints configured.")
		return 0
	}
	selected, err := a.promptChoice("Remove custom endpoint:", choices)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	removeIndex := indexes[selected]
	removed := cfg.Providers.Custom[removeIndex]
	cfg.Providers.Custom = append(cfg.Providers.Custom[:removeIndex], cfg.Providers.Custom[removeIndex+1:]...)
	if strings.EqualFold(cfg.EffectiveProvider(), "custom:"+removed.Name) {
		cfg.SetDefaultModel("", "")
	}
	if err := config.SaveConfig(cfg.Path, cfg); err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	fmt.Fprintf(a.stdout, "Removed custom endpoint: %s\nConfig: %s\n", removed.Name, cfg.Path)
	return 0
}

func (a *App) setModelFromArgs(cfg *config.Config, args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(a.stderr, "usage: selfmind model set <provider> <model> [--reasoning <level|auto>] [--service-tier <tier|auto>]")
		return 2
	}
	provider := strings.TrimSpace(args[0])
	model := strings.TrimSpace(args[1])
	if provider == "" || model == "" {
		fmt.Fprintln(a.stderr, "provider and model are required")
		return 2
	}
	reasoning, serviceTier, err := parseModelSetOptions(args[2:])
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 2
	}

	switch strings.ToLower(provider) {
	case "google", "gemini":
		provider = "google"
	default:
		if strings.HasPrefix(strings.ToLower(provider), "custom:") {
			name := strings.TrimPrefix(provider, "custom:")
			for i := range cfg.Providers.Custom {
				if strings.EqualFold(cfg.Providers.Custom[i].Name, name) {
					cfg.Providers.Custom[i].Model = model
					break
				}
			}
		} else if profile, ok := modelruntime.NewResolver(cfg).Registry().Resolve(provider); ok {
			endpoint := providerEndpointForModelCommand(cfg, profile.ID)
			endpoint.Model = ""
			endpoint.BaseURL = firstNonEmpty(endpoint.BaseURL, profile.BaseURL)
			endpoint.Protocol = modelruntime.NormalizeProtocol(firstNonEmpty(endpoint.Protocol, profile.Protocol))
			setProviderEndpointForModelCommand(cfg, profile.ID, endpoint)
			provider = profile.ID
		}
	}

	if err := validateModelOptions(provider, model, reasoning, serviceTier); err != nil {
		fmt.Fprintln(a.stderr, err)
		return 2
	}
	cfg.SetPrimaryModel(provider, model, reasoning)
	cfg.Models.Primary.ServiceTier = normalizeModelOption(serviceTier)
	if err := config.SaveConfig(cfg.Path, cfg); err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	fmt.Fprintf(a.stdout, "Saved model: %s / %s\n", provider, model)
	fmt.Fprintf(a.stdout, "Reasoning: %s\n", displayModelOption(reasoning))
	if serviceTier != "" {
		fmt.Fprintf(a.stdout, "Service tier: %s\n", displayModelOption(serviceTier))
	}
	fmt.Fprintf(a.stdout, "Config: %s\n", cfg.Path)
	return 0
}

func (a *App) printCurrentModel(cfg *config.Config) {
	primary := cfg.EffectivePrimary()
	fmt.Fprintf(a.stdout, "Current: provider=%s model=%s\n", blankAsDash(primary.Provider), blankAsDash(primary.Model))
	if descriptor, ok := modelruntime.DiscoverModelDescriptor(primary.Provider, primary.Model); ok {
		fmt.Fprintf(a.stdout, "Reasoning: %s\n", formatReasoning(primary.Reasoning, descriptor.DefaultReasoning))
		if primary.ServiceTier != "" || descriptor.DefaultServiceTier != "" {
			fmt.Fprintf(a.stdout, "Service tier: %s\n", formatModelDefault(primary.ServiceTier, descriptor.DefaultServiceTier))
		}
		if descriptor.ContextWindow > 0 {
			fmt.Fprintf(a.stdout, "Context: %d (%s)\n", descriptor.ContextWindow, descriptor.CapabilitySource)
		}
	} else {
		fmt.Fprintf(a.stdout, "Reasoning: %s\n", displayModelOption(primary.Reasoning))
		if primary.ServiceTier != "" {
			fmt.Fprintf(a.stdout, "Service tier: %s\n", displayModelOption(primary.ServiceTier))
		}
	}
	fmt.Fprintf(a.stdout, "Config: %s\n", cfg.Path)
}

func parseModelSetOptions(args []string) (reasoning, serviceTier string, err error) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--reasoning":
			if i+1 >= len(args) {
				return "", "", fmt.Errorf("--reasoning requires a value")
			}
			reasoning = strings.TrimSpace(args[i+1])
			i++
		case "--service-tier":
			if i+1 >= len(args) {
				return "", "", fmt.Errorf("--service-tier requires a value")
			}
			serviceTier = strings.TrimSpace(args[i+1])
			i++
		default:
			return "", "", fmt.Errorf("unknown model option: %s", args[i])
		}
	}
	return reasoning, serviceTier, nil
}

func validateModelOptions(provider, model, reasoning, serviceTier string) error {
	descriptor, ok := modelruntime.DiscoverModelDescriptor(provider, model)
	if !ok {
		return nil
	}
	if value := normalizeModelOption(reasoning); value != "" && len(descriptor.SupportedReasoning) > 0 && !containsFold(descriptor.SupportedReasoning, value) {
		return fmt.Errorf("reasoning %q is not supported by %s; supported: %s", value, model, strings.Join(descriptor.SupportedReasoning, ", "))
	}
	if value := normalizeModelOption(serviceTier); value != "" && len(descriptor.SupportedServiceTiers) > 0 && !containsFold(descriptor.SupportedServiceTiers, value) {
		return fmt.Errorf("service tier %q is not supported by %s; supported: %s", value, model, strings.Join(descriptor.SupportedServiceTiers, ", "))
	}
	return nil
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}

func normalizeModelOption(value string) string {
	value = strings.TrimSpace(value)
	if strings.EqualFold(value, "auto") {
		return ""
	}
	return value
}

func displayModelOption(value string) string {
	if normalized := normalizeModelOption(value); normalized != "" {
		return normalized
	}
	return "auto"
}

func formatReasoning(explicit, providerDefault string) string {
	return formatModelDefault(explicit, providerDefault)
}

func formatModelDefault(explicit, providerDefault string) string {
	if explicit = normalizeModelOption(explicit); explicit != "" {
		return explicit + " (explicit)"
	}
	if providerDefault = strings.TrimSpace(providerDefault); providerDefault != "" {
		return "auto (provider default: " + providerDefault + ")"
	}
	return "auto"
}

// redactHeaderValue masks secrets and account-scoped identity headers; other
// values are shown verbatim so compatibility overrides can be verified.
func redactHeaderValue(key, value string) string {
	lower := strings.ToLower(key)
	for _, marker := range []string{"key", "token", "secret", "authorization", "cookie", "password", "account", "user-id", "userid"} {
		if strings.Contains(lower, marker) {
			return "***"
		}
	}
	if len(value) > 120 {
		return value[:117] + "..."
	}
	return value
}

type modelCheckOptions struct {
	Live bool
	Role string
}

func parseModelCheckOptions(args []string) (modelCheckOptions, error) {
	var options modelCheckOptions
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--live":
			options.Live = true
		case "--role":
			i++
			if i >= len(args) || strings.TrimSpace(args[i]) == "" {
				return modelCheckOptions{}, fmt.Errorf("--role requires a configured role name")
			}
			options.Role = strings.TrimSpace(args[i])
		default:
			return modelCheckOptions{}, fmt.Errorf("unknown model check option: %s", args[i])
		}
	}
	return options, nil
}

func (a *App) checkCurrentModel(cfg *config.Config, options modelCheckOptions) int {
	if options.Role == "" || strings.EqualFold(options.Role, "primary") {
		a.printCurrentModel(cfg)
	} else {
		fmt.Fprintf(a.stdout, "Role: %s\n", options.Role)
	}
	rt, err := appcore.ResolveModelRuntime(a.ctx, cfg, options.Role)
	if err != nil {
		fmt.Fprintf(a.stderr, "Model check failed: %v\n", err)
		fmt.Fprintln(a.stderr, modelCheckFailureHint(options.Role))
		return 1
	}
	fmt.Fprintf(a.stdout, "Resolved: provider=%s model=%s protocol=%s\n", rt.Provider, rt.Model, rt.Protocol)
	fmt.Fprintf(a.stdout, "Base URL: %s\n", rt.BaseURL)
	fmt.Fprintf(a.stdout, "Credential: %s\n", blankAsDash(rt.CredentialSource))
	if rt.ContextLength > 0 {
		fmt.Fprintf(a.stdout, "Context length: %d (%s)\n", rt.ContextLength, blankAsDash(rt.ContextSource))
	} else {
		fmt.Fprintln(a.stdout, "Context length: unknown")
	}
	fmt.Fprintf(a.stdout, "Reasoning: %s\n", formatReasoning(rt.ReasoningEffort, rt.DefaultReasoning))
	if len(rt.ReasoningLevels) > 0 {
		fmt.Fprintf(a.stdout, "Reasoning levels: %s\n", strings.Join(rt.ReasoningLevels, ", "))
	}
	if rt.CapabilitySource != "" {
		fmt.Fprintf(a.stdout, "Capabilities: %s\n", rt.CapabilitySource)
	}
	printMaintenanceFallback(a.stdout, cfg, options.Role)
	fmt.Fprintf(a.stdout, "Quirks: auth=%s tool_schema=%s thinking=%s user_identity=%s system=%s http=%s prompt_cache=%s responses_store_false=%t responses_require_stream=%t\n",
		blankAsDash(rt.Quirks.AuthHeader),
		blankAsDash(rt.Quirks.ToolSchema),
		blankAsDash(rt.Quirks.ThinkingMode),
		effectiveUserIdentityDisplay(rt.Protocol, rt.Quirks.UserIdentityField),
		blankAsDash(rt.Quirks.SystemMessageMode),
		blankAsDash(rt.Quirks.HTTPVersion),
		promptCacheDisplay(rt.Provider, rt.Protocol, rt.Quirks.PromptCache),
		rt.Quirks.ResponsesStoreFalse,
		rt.Quirks.ResponsesRequireStream,
	)
	if userAgent := strings.TrimSpace(rt.Quirks.UserAgent); userAgent != "" {
		fmt.Fprintf(a.stdout, "Quirk user agent: %s\n", userAgent)
	}
	for _, warning := range modelruntime.QuirkDiagnostics(rt.Protocol, rt.Quirks) {
		fmt.Fprintf(a.stdout, "Warning: %s\n", warning)
	}
	if len(rt.Headers) > 0 {
		fmt.Fprintln(a.stdout, "Extra headers (merged; protocol headers like content-type/auth are added at request time):")
		origins := modelruntime.NewResolver(cfg).HeaderOrigins(rt.Provider, rt.Headers)
		keys := make([]string, 0, len(rt.Headers))
		for key := range rt.Headers {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fmt.Fprintf(a.stdout, "  %s: %s  [%s]\n", key, redactHeaderValue(key, rt.Headers[key]), origins[key])
		}
	}
	if keys := sortedInterfaceMapKeys(rt.ExtraBody); len(keys) > 0 {
		fmt.Fprintf(a.stdout, "Extra body keys: %s\n", strings.Join(keys, ", "))
	}
	if keys := sortedInterfaceMapKeys(rt.ExtraQuery); len(keys) > 0 {
		fmt.Fprintf(a.stdout, "Extra query keys: %s\n", strings.Join(keys, ", "))
	}
	if rt.Quirks.DisableHTTP2 || strings.EqualFold(rt.Quirks.HTTPVersion, "http1") {
		fmt.Fprintln(a.stdout, "Transport: TLS ALPN restricted to http/1.1")
	}
	if rt.TokenGetter != nil {
		fmt.Fprintln(a.stdout, "Token getter: configured")
	}
	if rt.TokenRefresher != nil {
		fmt.Fprintln(a.stdout, "Token refresher: configured")
	}
	if options.Live {
		fmt.Fprintln(a.stdout, "Live probe: sending one bounded request")
		probe := appcore.ProbeResolvedModelForRole(a.ctx, rt, options.Role)
		if probe.Err != nil {
			fmt.Fprintf(a.stderr, "Live probe failed after %s: %s\n", probe.Latency.Round(time.Millisecond), tools.RedactSensitive(probe.Err.Error()))
			return 1
		}
		toolStatus := "skipped (transport does not advertise native tools)"
		if probe.NativeToolsTested {
			toolStatus = "passed"
		}
		fmt.Fprintf(a.stdout, "Live probe: OK in %s\n", probe.Latency.Round(time.Millisecond))
		fmt.Fprintf(a.stdout, "  transport: passed\n  native tool schema: %s\n", toolStatus)
		if probe.ThinkingToolLoopTested {
			thinkingStatus := "failed"
			if probe.ThinkingToolLoopPassed {
				thinkingStatus = "passed"
			}
			fmt.Fprintf(a.stdout, "  thinking tool loop: %s\n", thinkingStatus)
		}
		if probe.MaintenanceContractTested {
			contractStatus := "failed"
			if probe.MaintenanceContractPassed {
				contractStatus = "passed"
			}
			fmt.Fprintf(a.stdout, "  maintenance contract: %s\n", contractStatus)
		}
	}
	return 0
}

func sortedInterfaceMapKeys(values map[string]interface{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		if key = strings.TrimSpace(key); key != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func effectiveUserIdentityDisplay(protocol, field string) string {
	field = strings.ToLower(strings.TrimSpace(field))
	if field == "" || field == "off" {
		return "off"
	}
	if field == "auto" {
		if modelruntime.NormalizeProtocol(protocol) == modelruntime.ProtocolAnthropic {
			return "auto->metadata.user_id"
		}
		return "auto->user_id"
	}
	return field
}

func promptCacheDisplay(provider, protocol string, enabled bool) string {
	if modelruntime.NormalizeProtocol(protocol) == modelruntime.ProtocolAnthropic {
		if enabled {
			return "cache_control:on"
		}
		return "cache_control:off"
	}
	if strings.EqualFold(strings.TrimSpace(provider), "deepseek") {
		return "provider-managed"
	}
	return "n/a"
}

// printMaintenanceFallback makes the resolved fallback position visible. A
// chain that collapses onto one physical route is the normal outcome for an
// installation with a single provider, and saying so is the point: otherwise
// the only record is an INFO log nobody reads.
func printMaintenanceFallback(out io.Writer, cfg *config.Config, role string) {
	summary := appcore.DescribeMaintenanceFallback(cfg, role)
	if !summary.Chained {
		return
	}
	switch {
	case summary.Provider != "":
		fmt.Fprintf(out, "Maintenance fallback: %s -> provider=%s model=%s\n",
			summary.Slot, summary.Provider, blankAsDash(summary.Model))
	case summary.Collapsed:
		fmt.Fprintln(out, "Maintenance fallback: none (every fallback position resolves to this same endpoint and credential)")
	default:
		fmt.Fprintln(out, "Maintenance fallback: none (configure models.auxiliary on a different provider to add one)")
	}
}

func modelCheckFailureHint(role string) string {
	if strings.EqualFold(strings.TrimSpace(role), "auxiliary") {
		return "Hint: run `selfmind setup`, or configure `models.auxiliary` and restart the gateway."
	}
	if strings.TrimSpace(role) != "" && !strings.EqualFold(role, "primary") {
		return "Hint: configure `models.roles." + strings.TrimSpace(role) + "` (or `models.auxiliary` for a background role), then restart the gateway."
	}
	return "Hint: run `selfmind model` and enter the API key, or set the provider API key environment variable before starting SelfMind."
}

func (a *App) printConfiguredProviders(cfg *config.Config) {
	a.printCurrentModel(cfg)
	fmt.Fprintln(a.stdout)
	fmt.Fprintf(a.stdout, "OpenAI: %s %s\n", configuredMark(cfg.Providers.OpenAI.APIKey), cfg.Providers.OpenAI.BaseURL)
	fmt.Fprintf(a.stdout, "Anthropic: %s %s\n", configuredMark(cfg.Providers.Anthropic.APIKey), cfg.Providers.Anthropic.BaseURL)
	fmt.Fprintf(a.stdout, "Google: %s %s\n", configuredMark(cfg.Providers.Google.APIKey), cfg.Providers.Google.BaseURL)
	if cfg.Providers.OpenRouterAPIKey != "" {
		fmt.Fprintf(a.stdout, "OpenRouter legacy: %s\n", configuredMark(cfg.Providers.OpenRouterAPIKey))
	}
	if cfg.Providers.MiniMaxAPIKey != "" {
		fmt.Fprintf(a.stdout, "MiniMax legacy: %s\n", configuredMark(cfg.Providers.MiniMaxAPIKey))
	}
	for name, ep := range cfg.ProviderProfiles {
		fmt.Fprintf(a.stdout, "Provider %s: %s %s protocol=%s\n", name, configuredMark(ep.APIKey), ep.BaseURL, ep.Protocol)
	}
	for _, cp := range cfg.Providers.Custom {
		fmt.Fprintf(a.stdout, "Custom %s: %s %s\n", cp.Name, configuredMark(cp.APIKey), cp.BaseURL)
	}
}

func (a *App) promptChoice(title string, labels []string) (int, error) {
	fmt.Fprintln(a.stdout, title)
	for i, label := range labels {
		fmt.Fprintf(a.stdout, "  %d. %s\n", i+1, label)
	}
	for {
		raw, err := a.promptInput("Select", "1")
		if err != nil {
			return -1, err
		}
		index, err := strconv.Atoi(strings.TrimSpace(raw))
		if err == nil && index >= 1 && index <= len(labels) {
			return index - 1, nil
		}
		fmt.Fprintf(a.stdout, "Enter a number from 1 to %d.\n", len(labels))
	}
}

func (a *App) promptModel(models []string, current string) (string, error) {
	models = uniqueSorted(models)
	if len(models) > 0 {
		labels := append([]string{}, models...)
		labels = append(labels, "Enter model manually")
		index, err := a.promptChoice("Choose a model:", labels)
		if err != nil {
			return "", err
		}
		if index < len(models) {
			return models[index], nil
		}
	}
	return a.promptInput("Model", current)
}

func (a *App) promptAPIKey(label, current string) (string, error) {
	currentLabel := "empty"
	if current != "" {
		currentLabel = maskSecret(current)
	}
	raw, err := a.promptInput(fmt.Sprintf("API key for %s (blank keeps %s, '-' clears)", label, currentLabel), "")
	if err != nil {
		return "", err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return current, nil
	}
	if raw == "-" {
		return "", nil
	}
	return raw, nil
}

func (a *App) promptInput(label, defaultValue string) (string, error) {
	if defaultValue == "" {
		fmt.Fprintf(a.stdout, "%s: ", label)
	} else {
		fmt.Fprintf(a.stdout, "%s [%s]: ", label, defaultValue)
	}
	if a.input == nil {
		a.input = bufio.NewReader(a.stdin)
	}
	raw, err := a.input.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultValue, nil
	}
	return raw, nil
}

func fetchOpenAICompatibleModels(ctx context.Context, baseURL, apiKey string) ([]string, error) {
	return fetchModelIDs(ctx, openAIModelsURL(baseURL), apiKey, map[string]string{})
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
	var ids []string
	var walk func(interface{})
	walk = func(v interface{}) {
		switch item := v.(type) {
		case map[string]interface{}:
			if id := stringFromInterface(item["id"]); id != "" {
				ids = append(ids, cleanModelID(id))
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

func openAIModelsURL(baseURL string) string {
	return normalizeOpenAIRoot(baseURL) + "/models"
}

func normalizeOpenAIRoot(baseURL string) string {
	root := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if root == "" {
		return "https://api.openai.com/v1"
	}
	lower := strings.ToLower(root)
	if strings.HasSuffix(lower, "/chat/completions") {
		root = root[:len(root)-len("/chat/completions")]
	}
	return strings.TrimRight(root, "/")
}

func providerEndpointForModelCommand(cfg *config.Config, provider string) config.ProviderEndpoint {
	// The model command reads old flat keys and new provider_profiles through
	// the same view so users can upgrade config.yaml gradually.
	switch modelruntime.NormalizeProviderID(provider) {
	case "openai":
		return cfg.Providers.OpenAI
	case "anthropic", "claude-code":
		return cfg.Providers.Anthropic
	case "google", "gemini", "gemini-cli":
		return cfg.Providers.Google
	case "openrouter":
		if ep, ok := cfg.ProviderProfiles["openrouter"]; ok {
			return ep
		}
		return config.ProviderEndpoint{APIKey: cfg.Providers.OpenRouterAPIKey}
	case "minimax":
		if ep, ok := cfg.ProviderProfiles["minimax"]; ok {
			return ep
		}
		return config.ProviderEndpoint{APIKey: cfg.Providers.MiniMaxAPIKey}
	case "minimax-cn", "minimax-oauth":
		if ep, ok := cfg.ProviderProfiles[modelruntime.NormalizeProviderID(provider)]; ok {
			return ep
		}
		return config.ProviderEndpoint{}
	default:
		if ep, ok := cfg.ProviderProfiles[modelruntime.NormalizeProviderID(provider)]; ok {
			return ep
		}
		return config.ProviderEndpoint{}
	}
}

func setProviderEndpointForModelCommand(cfg *config.Config, provider string, endpoint config.ProviderEndpoint) {
	id := modelruntime.NormalizeProviderID(provider)
	switch id {
	case "openai":
		cfg.Providers.OpenAI = endpoint
		return
	case "anthropic":
		cfg.Providers.Anthropic = endpoint
		return
	case "google", "gemini":
		cfg.Providers.Google = endpoint
		return
	case "openrouter":
		cfg.Providers.OpenRouterAPIKey = endpoint.APIKey
	case "minimax":
		cfg.Providers.MiniMaxAPIKey = endpoint.APIKey
	}
	// Non-core providers are persisted as profiles, which lets new model vendors
	// be added locally without another binary release.
	if cfg.ProviderProfiles == nil {
		cfg.ProviderProfiles = make(map[string]config.ProviderEndpoint)
	}
	cfg.ProviderProfiles[id] = endpoint
}

func externalLoginHint(provider string) string {
	switch modelruntime.NormalizeProviderID(provider) {
	case "codex-cli":
		return "Run `codex` and sign in first, or set CODEX_ACCESS_TOKEN."
	case "claude-code":
		return "Run Claude Code login first, or set CLAUDE_CODE_OAUTH_TOKEN / ANTHROPIC_TOKEN."
	case "gemini-cli":
		return "Run Gemini CLI login first, or set GEMINI_OAUTH_ACCESS_TOKEN."
	case "qwen-cli":
		return "Run `qwen auth qwen-oauth` first, or set QWEN_ACCESS_TOKEN."
	case "minimax-oauth":
		return "Run `selfmind auth login minimax-oauth` first, or set a MiniMax API key on the `minimax` provider."
	default:
		return "Sign in with the matching CLI first, or configure an API key."
	}
}

func firstModelChoice(models []string) string {
	if len(models) == 0 {
		return ""
	}
	return strings.TrimSpace(models[0])
}

func upsertCustomProvider(cfg *config.Config, cp config.CustomProvider) {
	for i, existing := range cfg.Providers.Custom {
		if strings.EqualFold(existing.Name, cp.Name) || strings.EqualFold(strings.TrimRight(existing.BaseURL, "/"), strings.TrimRight(cp.BaseURL, "/")) {
			cfg.Providers.Custom[i] = cp
			return
		}
	}
	cfg.Providers.Custom = append(cfg.Providers.Custom, cp)
}

func uniqueCustomName(cfg *config.Config, name, baseURL string) string {
	name = sanitizeCustomName(name)
	if name == "" {
		name = customNameFromURL(baseURL)
	}
	for _, cp := range cfg.Providers.Custom {
		if strings.EqualFold(strings.TrimRight(cp.BaseURL, "/"), strings.TrimRight(baseURL, "/")) {
			return cp.Name
		}
	}
	used := map[string]bool{}
	for _, cp := range cfg.Providers.Custom {
		used[strings.ToLower(cp.Name)] = true
	}
	if !used[strings.ToLower(name)] {
		return name
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", name, i)
		if !used[strings.ToLower(candidate)] {
			return candidate
		}
	}
}

func customNameFromURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "custom"
	}
	host := strings.Split(parsed.Host, ":")[0]
	host = strings.TrimPrefix(host, "www.")
	parts := strings.Split(host, ".")
	if len(parts) > 0 && parts[0] != "" {
		return sanitizeCustomName(parts[0])
	}
	return "custom"
}

func sanitizeCustomName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	re := regexp.MustCompile(`[^a-z0-9_-]+`)
	name = re.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-_")
	return name
}

func choiceLabels(choices []modelChoice) []string {
	labels := make([]string, 0, len(choices))
	for _, choice := range choices {
		labels = append(labels, choice.Label)
	}
	return labels
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

func cleanModelID(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "models/") {
		value = strings.TrimPrefix(value, "models/")
	}
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

func configuredMark(value string) string {
	if strings.TrimSpace(value) == "" {
		return "[not configured]"
	}
	return "[configured]"
}

func blankAsDash(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	return value
}

func maskSecret(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 8 {
		return "********"
	}
	return value[:4] + "..." + value[len(value)-4:]
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
