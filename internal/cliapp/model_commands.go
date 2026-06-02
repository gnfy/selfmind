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

	"selfmind/internal/platform/config"
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
	case "list":
		a.printConfiguredProviders(cfg)
		return true, 0
	case "set":
		return true, a.setModelFromArgs(cfg, args[1:])
	default:
		fmt.Fprintln(a.stderr, "usage: selfmind model [current|list|set <provider> <model>]")
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
	choices := []modelChoice{
		{ID: "openai", Label: "OpenAI", Kind: "builtin"},
		{ID: "anthropic", Label: "Anthropic", Kind: "builtin"},
		{ID: "google", Label: "Google", Kind: "builtin"},
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
	choices = append(choices, modelChoice{ID: "custom", Label: "Custom endpoint (enter URL manually)", Kind: "custom_new"})
	if len(cfg.Providers.Custom) > 0 {
		choices = append(choices, modelChoice{ID: "remove_custom", Label: "Remove a custom endpoint", Kind: "remove_custom"})
	}
	choices = append(choices, modelChoice{ID: "skip", Label: "Cancel", Kind: "skip"})
	return choices
}

func (a *App) configureBuiltinProvider(cfg *config.Config, provider string) int {
	endpoint := builtinEndpoint(cfg, provider)
	key, err := a.promptAPIKey(provider, endpoint.APIKey)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	endpoint.APIKey = key

	models, err := fetchProviderModels(a.ctx, provider, endpoint.BaseURL, key)
	if err != nil {
		fmt.Fprintf(a.stdout, "Could not load remote model list: %v\n", err)
	}
	model, err := a.promptModel(models, firstNonEmpty(endpoint.Model, cfg.EffectiveModel(), fallbackModels(provider)[0]))
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	endpoint.Model = model

	setBuiltinEndpoint(cfg, provider, endpoint)
	cfg.SetDefaultModel(provider, model)
	if err := config.SaveConfig(cfg.Path, cfg); err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	fmt.Fprintf(a.stdout, "Saved model: %s / %s\nConfig: %s\n", provider, model, cfg.Path)
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
	cfg.SetDefaultModel("custom:"+cp.Name, model)
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

	contextRaw, err := a.promptInput("Context length (optional)", "")
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	contextLength := 0
	if strings.TrimSpace(contextRaw) != "" {
		contextLength, _ = strconv.Atoi(strings.TrimSpace(contextRaw))
	}

	cp := config.CustomProvider{
		Name:     name,
		BaseURL:  normalizeOpenAIRoot(baseURL),
		APIKey:   key,
		Protocol: "openai_compatible",
		Model:    model,
	}
	if contextLength > 0 && model != "" {
		cp.Models = map[string]config.CustomModelProperties{
			model: {ContextLength: contextLength},
		}
	}
	upsertCustomProvider(cfg, cp)
	cfg.SetDefaultModel("custom:"+name, model)
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
		fmt.Fprintln(a.stderr, "usage: selfmind model set <provider> <model>")
		return 2
	}
	provider := strings.TrimSpace(args[0])
	model := strings.TrimSpace(strings.Join(args[1:], " "))
	if provider == "" || model == "" {
		fmt.Fprintln(a.stderr, "provider and model are required")
		return 2
	}

	switch strings.ToLower(provider) {
	case "openai":
		cfg.Providers.OpenAI.Model = model
	case "anthropic":
		cfg.Providers.Anthropic.Model = model
	case "google", "gemini":
		provider = "google"
		cfg.Providers.Google.Model = model
	default:
		if strings.HasPrefix(strings.ToLower(provider), "custom:") {
			name := strings.TrimPrefix(provider, "custom:")
			for i := range cfg.Providers.Custom {
				if strings.EqualFold(cfg.Providers.Custom[i].Name, name) {
					cfg.Providers.Custom[i].Model = model
					break
				}
			}
		}
	}

	cfg.SetDefaultModel(provider, model)
	if err := config.SaveConfig(cfg.Path, cfg); err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	fmt.Fprintf(a.stdout, "Saved model: %s / %s\nConfig: %s\n", provider, model, cfg.Path)
	return 0
}

func (a *App) printCurrentModel(cfg *config.Config) {
	fmt.Fprintf(a.stdout, "Current: provider=%s model=%s\n", blankAsDash(cfg.EffectiveProvider()), blankAsDash(cfg.EffectiveModel()))
	fmt.Fprintf(a.stdout, "Config: %s\n", cfg.Path)
}

func (a *App) printConfiguredProviders(cfg *config.Config) {
	a.printCurrentModel(cfg)
	fmt.Fprintln(a.stdout)
	fmt.Fprintf(a.stdout, "OpenAI: %s %s\n", configuredMark(cfg.Providers.OpenAI.APIKey), cfg.Providers.OpenAI.BaseURL)
	fmt.Fprintf(a.stdout, "Anthropic: %s %s\n", configuredMark(cfg.Providers.Anthropic.APIKey), cfg.Providers.Anthropic.BaseURL)
	fmt.Fprintf(a.stdout, "Google: %s %s\n", configuredMark(cfg.Providers.Google.APIKey), cfg.Providers.Google.BaseURL)
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

func fetchProviderModels(ctx context.Context, provider, baseURL, apiKey string) ([]string, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "openai":
		return fetchOpenAICompatibleModels(ctx, baseURL, apiKey)
	case "anthropic":
		return fetchAnthropicModels(ctx, baseURL, apiKey)
	case "google", "gemini":
		return fetchGoogleModels(ctx, baseURL, apiKey)
	default:
		return nil, fmt.Errorf("unsupported provider: %s", provider)
	}
}

func fetchOpenAICompatibleModels(ctx context.Context, baseURL, apiKey string) ([]string, error) {
	return fetchModelIDs(ctx, openAIModelsURL(baseURL), apiKey, map[string]string{})
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
	headers := map[string]string{"anthropic-version": "2023-06-01"}
	return fetchModelIDs(ctx, root+"/v1/models", apiKey, headers)
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
	return fetchModelIDs(ctx, modelURL, "", map[string]string{})
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

func builtinEndpoint(cfg *config.Config, provider string) config.ProviderEndpoint {
	switch strings.ToLower(provider) {
	case "anthropic":
		return cfg.Providers.Anthropic
	case "google", "gemini":
		return cfg.Providers.Google
	default:
		return cfg.Providers.OpenAI
	}
}

func setBuiltinEndpoint(cfg *config.Config, provider string, endpoint config.ProviderEndpoint) {
	switch strings.ToLower(provider) {
	case "anthropic":
		cfg.Providers.Anthropic = endpoint
	case "google", "gemini":
		cfg.Providers.Google = endpoint
	default:
		cfg.Providers.OpenAI = endpoint
	}
}

func fallbackModels(provider string) []string {
	switch strings.ToLower(provider) {
	case "anthropic":
		return []string{"claude-3-5-sonnet-20241022", "claude-3-5-haiku-20241022"}
	case "google", "gemini":
		return []string{"gemini-1.5-pro", "gemini-1.5-flash"}
	default:
		return []string{"gpt-4o", "gpt-4o-mini"}
	}
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
