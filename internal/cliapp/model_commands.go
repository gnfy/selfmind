package cliapp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	appcore "selfmind/internal/app"
	"selfmind/internal/gateway/api"
	"selfmind/internal/modelchange"
	"selfmind/internal/modelruntime"
	"selfmind/internal/platform/config"
	gatewayrt "selfmind/internal/runtime/gateway"
)

type modelChoice struct {
	ID          string
	Label       string
	Kind        string
	CustomIndex int
	Ready       bool
}

var errModelChangeEndpointUnavailable = errors.New("running gateway does not support model transactions")

func (a *App) runModelCommandIfRequested() (bool, int) {
	if len(a.args) < 2 || a.args[1] != "model" {
		return false, 0
	}
	if len(a.args) != 2 {
		fmt.Fprintln(a.stderr, "Usage: selfmind model")
		return true, 2
	}

	_, err := config.LoadConfig(config.Options{Path: a.configPath, CreateIfMissing: true})
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return true, 1
	}
	a.modelManagerOnly = true
	return true, a.runTUI()
}

func (a *App) modelChoices(cfg *config.Config) []modelChoice {
	base := []modelChoice{
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
	for index := range base {
		base[index].Ready = a.modelChoiceReady(cfg, base[index])
		if base[index].Ready {
			base[index].Label += " (ready)"
		}
	}
	choices := base
	for i, cp := range cfg.Providers.Custom {
		name := strings.TrimSpace(cp.Name)
		if name == "" {
			continue
		}
		choices = append(choices, modelChoice{
			ID:          name,
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

func (a *App) modelChoiceReady(cfg *config.Config, choice modelChoice) bool {
	if cfg == nil || choice.Kind != "builtin" {
		return false
	}
	resolver := modelruntime.NewResolver(cfg)
	profile, ok := resolver.Registry().Resolve(choice.ID)
	if !ok {
		return false
	}
	runtime, err := resolver.Resolve(a.ctx, modelruntime.Selection{
		Provider: profile.ID,
		Model:    firstModelChoice(profile.FallbackModels),
	})
	return err == nil && strings.TrimSpace(runtime.APIKey) != ""
}

func (a *App) configureBuiltinProvider(cfg *config.Config, provider string) int {
	return a.configureBuiltinProviderFor(cfg, provider, "primary")
}

func (a *App) configureBuiltinProviderFor(cfg *config.Config, provider, target string) int {
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
		currentKey := endpoint.APIKey
		if strings.TrimSpace(currentKey) == "" {
			if existing, resolveErr := resolver.Resolve(a.ctx, modelruntime.Selection{
				Provider: profile.ID, Model: firstNonEmpty(endpoint.Model, cfg.EffectiveModel()), BaseURL: endpoint.BaseURL,
			}); resolveErr == nil {
				currentKey = existing.APIKey
			}
		}
		var err error
		key, err = a.promptAPIKey(profile.DisplayName, currentKey)
		if err != nil {
			fmt.Fprintln(a.stderr, err)
			return 1
		}
		if err := modelruntime.NewCredentialStore(cfg.Auth.CredentialsFile).SaveAPIKey(profile.ID, key); err != nil {
			fmt.Fprintf(a.stderr, "Save model credential: %v\n", err)
			return 1
		}
		endpoint.APIKey = ""
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
	if strings.EqualFold(strings.TrimRight(endpoint.BaseURL, "/"), strings.TrimRight(profile.BaseURL, "/")) {
		endpoint.BaseURL = ""
	}
	if modelruntime.NormalizeProtocol(endpoint.Protocol) == modelruntime.NormalizeProtocol(profile.Protocol) {
		endpoint.Protocol = ""
	}

	setProviderEndpointForModelCommand(cfg, profile.ID, endpoint)
	if profile.AuthType != modelruntime.AuthExternalOAuth && profile.AuthType != modelruntime.AuthMiniMaxOAuth {
		clearProviderCredentialForModelCommand(cfg, profile.ID)
	}
	return a.finalizeInteractiveModel(cfg, target, profile.ID, model)
}

func (a *App) configureSavedCustomProvider(cfg *config.Config, index int) int {
	return a.configureSavedCustomProviderFor(cfg, index, "primary")
}

func (a *App) configureSavedCustomProviderFor(cfg *config.Config, index int, target string) int {
	if index < 0 || index >= len(cfg.Providers.Custom) {
		fmt.Fprintln(a.stderr, "custom provider not found")
		return 1
	}
	cp := cfg.Providers.Custom[index]
	currentKey := cp.APIKey
	if strings.TrimSpace(currentKey) == "" {
		if resolved, resolveErr := modelruntime.NewResolver(cfg).Resolve(a.ctx, modelruntime.Selection{
			Provider: cp.Name, Model: cp.Model,
		}); resolveErr == nil {
			currentKey = resolved.APIKey
		}
	}
	key, err := a.promptAPIKey(cp.Name, currentKey)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	if err := modelruntime.NewCredentialStore(cfg.Auth.CredentialsFile).SaveAPIKey(cp.Name, key); err != nil {
		fmt.Fprintf(a.stderr, "Save model credential: %v\n", err)
		return 1
	}
	cp.APIKey = ""

	models, err := fetchOpenAICompatibleModels(a.ctx, cp.BaseURL, key)
	if err != nil {
		fmt.Fprintf(a.stdout, "Could not load remote model list: %v\n", err)
	}
	model, err := a.promptModel(models, firstNonEmpty(cp.Model, cfg.EffectiveModel(), ""))
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	cp.Model = ""
	if cp.Protocol == "" {
		cp.Protocol = "openai-compatible"
	}
	cfg.Providers.Custom[index] = cp
	return a.finalizeInteractiveModel(cfg, target, cp.Name, model)
}

func (a *App) configureCustomEndpoint(cfg *config.Config) int {
	return a.configureCustomEndpointFor(cfg, "primary")
}

func (a *App) configureCustomEndpointFor(cfg *config.Config, target string) int {
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
		Protocol: "openai-compatible",
		Auth:     "bearer",
	}
	if err := modelruntime.NewCredentialStore(cfg.Auth.CredentialsFile).SaveAPIKey(name, key); err != nil {
		fmt.Fprintf(a.stderr, "Save model credential: %v\n", err)
		return 1
	}
	upsertCustomProvider(cfg, cp)
	return a.finalizeInteractiveModel(cfg, target, name, model)
}

func (a *App) finalizeInteractiveModel(cfg *config.Config, target, provider, model string) int {
	if err := config.SaveConfig(cfg.Path, cfg); err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	route := modelchange.RoutePrimary
	if strings.EqualFold(strings.TrimSpace(target), "auxiliary") {
		route = modelchange.RouteAuxiliary
	}
	candidate, err := modelchange.BuildCandidate(modelchange.SnapshotFromConfig(cfg), modelchange.SelectionPatch{
		Route: route, Provider: provider, Model: model,
	})
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	receipt, err := a.commitModelCandidate(cfg, route, provider, model, modelchange.OptionalValue{}, modelchange.OptionalValue{}, candidate)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	notices := candidate.Notices
	if receipt.Online {
		notices = receipt.Notices
	}
	for _, notice := range notices {
		fmt.Fprintf(a.stdout, "Notice: %s\n", notice)
	}
	fmt.Fprintf(a.stdout, "Saved %s model: %s / %s\n", modelTargetLabel(target), receipt.Selection.Provider, receipt.Selection.Model)
	if receipt.Online && receipt.RestartScheduled {
		fmt.Fprintf(a.stdout, "Change: %s (validated; safe restart scheduled)\n", receipt.ChangeID)
	} else if receipt.Online {
		fmt.Fprintf(a.stdout, "Change: %s (validated and saved; run `selfmind gateway restart --drain` to apply)\n", receipt.ChangeID)
	} else if receipt.LegacyDaemon {
		fmt.Fprintf(a.stdout, "State: saved, not applied (the running daemon needs an upgrade or `selfmind gateway restart --drain`)\n")
	} else {
		fmt.Fprintln(a.stdout, "State: saved, not applied (daemon is stopped; startup will verify again)")
	}
	fmt.Fprintf(a.stdout, "Config: %s\n", cfg.Path)
	return 0
}

func modelTargetLabel(target string) string {
	if strings.EqualFold(strings.TrimSpace(target), "auxiliary") {
		return "background"
	}
	return "primary"
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
	if strings.EqualFold(strings.TrimPrefix(cfg.EffectiveProvider(), "custom:"), removed.Name) {
		cfg.SetDefaultModel("", "")
	}
	if err := config.SaveConfig(cfg.Path, cfg); err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	fmt.Fprintf(a.stdout, "Removed custom endpoint: %s\nConfig: %s\n", removed.Name, cfg.Path)
	return 0
}

func (a *App) printCurrentModel(cfg *config.Config) {
	primary := cfg.EffectivePrimary()
	fmt.Fprintf(a.stdout, "Primary: provider=%s model=%s\n", blankAsDash(primary.Provider), blankAsDash(primary.Model))
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
	auxiliary := cfg.EffectiveAuxiliary()
	fmt.Fprintf(a.stdout, "Background: provider=%s model=%s\n", blankAsDash(auxiliary.Provider), blankAsDash(auxiliary.Model))
	if descriptor, ok := modelruntime.DiscoverModelDescriptor(auxiliary.Provider, auxiliary.Model); ok {
		fmt.Fprintf(a.stdout, "Background reasoning: %s\n", formatReasoning(auxiliary.Reasoning, descriptor.DefaultReasoning))
		if auxiliary.ServiceTier != "" || descriptor.DefaultServiceTier != "" {
			fmt.Fprintf(a.stdout, "Background service tier: %s\n", formatModelDefault(auxiliary.ServiceTier, descriptor.DefaultServiceTier))
		}
	} else {
		fmt.Fprintf(a.stdout, "Background reasoning: %s\n", displayModelOption(auxiliary.Reasoning))
		if auxiliary.ServiceTier != "" {
			fmt.Fprintf(a.stdout, "Background service tier: %s\n", displayModelOption(auxiliary.ServiceTier))
		}
	}
	if status, err := (&modelchange.Service{ConfigPath: cfg.Path}).Inspect(); err == nil {
		if status.Configured != status.Running {
			fmt.Fprintf(a.stdout, "Running primary: provider=%s model=%s reasoning=%s\n", blankAsDash(status.Running.Primary.Provider), blankAsDash(status.Running.Primary.Model), displayModelOption(status.Running.Primary.Reasoning))
			fmt.Fprintf(a.stdout, "Running background: provider=%s model=%s reasoning=%s\n", blankAsDash(status.Running.Auxiliary.Provider), blankAsDash(status.Running.Auxiliary.Model), displayModelOption(status.Running.Auxiliary.Reasoning))
		}
		if status.Pending != nil {
			fmt.Fprintf(a.stdout, "Pending: %s (%s)\n", status.Pending.ID, status.Pending.Status)
		}
		fmt.Fprintf(a.stdout, "Generation: %d\n", status.Generation)
	}
	fmt.Fprintf(a.stdout, "Config: %s\n", cfg.Path)
}

func optionalPointer(value modelchange.OptionalValue) *string {
	if !value.Set {
		return nil
	}
	copy := value.Value
	return &copy
}

func (a *App) modelChangeValidator() modelchange.Validator {
	if a != nil && a.modelChangeValidate != nil {
		return a.modelChangeValidate
	}
	return appcore.ValidateModelChange
}

// applyModelThroughDaemon keeps the running daemon as the sole online writer.
// A missing live owner is not an error: the caller falls back to an offline
// candidate that startup must probe again before accepting as running.
func (a *App) applyModelThroughDaemon(request api.ModelChangeRequest) (api.ModelChangeResponse, bool, error) {
	if a == nil || !a.gatewayTargetIsLocal() {
		return api.ModelChangeResponse{}, false, nil
	}
	if _, ok := a.modelDaemonRecord(); !ok {
		return api.ModelChangeResponse{}, false, nil
	}
	result, err := a.requestModelChange(a.ctx, request)
	if errors.Is(err, errModelChangeEndpointUnavailable) {
		return api.ModelChangeResponse{}, false, nil
	}
	if err == nil && result.Change == nil {
		err = fmt.Errorf("gateway returned no model change receipt")
	}
	return result, true, err
}

func (a *App) requestModelChange(parent context.Context, request api.ModelChangeRequest) (api.ModelChangeResponse, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return api.ModelChangeResponse{}, err
	}
	ctx, cancel := contextWithTimeout(parent, 60*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.gatewayURL()+"/v1/gateway/model/change", bytes.NewReader(body))
	if err != nil {
		return api.ModelChangeResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	a.attachGatewayAuth(httpReq)
	a.attachLocalControlAuth(httpReq)
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return api.ModelChangeResponse{}, err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return api.ModelChangeResponse{}, readErr
	}
	if resp.StatusCode >= http.StatusBadRequest {
		if resp.StatusCode == http.StatusNotFound {
			return api.ModelChangeResponse{}, fmt.Errorf("%w", errModelChangeEndpointUnavailable)
		}
		return api.ModelChangeResponse{}, fmt.Errorf("%s", gatewayErrorLine(resp.Status, data))
	}
	var result api.ModelChangeResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return api.ModelChangeResponse{}, err
	}
	return result, nil
}

func (a *App) performModelRecovery(cfg *config.Config, action, id string) (*modelchange.Service, string, error) {
	service := &modelchange.Service{ConfigPath: cfg.Path}
	status, err := service.Inspect()
	if err != nil {
		return nil, "", err
	}
	if status.Pending == nil || status.Pending.Status != modelchange.StatusRecoveryRequired {
		return nil, "", fmt.Errorf("there is no model change requiring recovery")
	}
	if strings.TrimSpace(id) != "" && id != status.Pending.ID {
		return nil, "", fmt.Errorf("pending model change %q was not found", id)
	}
	changeID := status.Pending.ID
	switch action {
	case "retry":
		if _, err := service.RetryRecovery(changeID); err != nil {
			return nil, "", err
		}
		if err := gatewayrt.SpawnRestartHelper(cfg.Path, a.gatewayDataDir(), changeID); err != nil {
			_, _ = service.MarkRecoveryRequired(changeID, err)
			return nil, "", err
		}
		return service, changeID, nil
	case "restore":
		if err := a.stopGatewayForModelRestore(); err != nil {
			return nil, "", err
		}
		if _, err := service.RestorePrevious(changeID); err != nil {
			_ = a.startGatewayAfterModelRestore()
			return nil, "", err
		}
		if err := a.startGatewayAfterModelRestore(); err != nil {
			return nil, "", err
		}
		return service, changeID, nil
	default:
		return nil, "", fmt.Errorf("unknown model recovery action %q", action)
	}
}

func (a *App) stopGatewayForModelRestore() error {
	if a != nil && a.modelRecoveryStop != nil {
		return a.modelRecoveryStop()
	}
	timeout := gatewayrt.ResolveDrainTimeout() + 10*time.Second
	ctx, cancel := contextWithTimeout(a.ctx, timeout)
	err := gatewayrt.RequestShutdown(ctx, gatewayrt.StopOptions{
		URL: a.gatewayURL(), DataDir: a.gatewayDataDir(), Timeout: timeout,
		Reason: api.ShutdownReasonServiceReconcile, WaitForSafeBoundary: true,
	})
	cancel()
	if err != nil {
		if errors.Is(err, gatewayrt.ErrShutdownDeferred) || errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("model restore deferred while foreground work is active; retry after it finishes")
		}
		return fmt.Errorf("stop the failed model runtime safely: %w", err)
	}
	releaseCtx, releaseCancel := contextWithTimeout(a.ctx, 10*time.Second)
	defer releaseCancel()
	if err := gatewayrt.WaitForOwnerRelease(releaseCtx, a.gatewayDataDir()); err != nil {
		return fmt.Errorf("wait for the failed model runtime to release ownership: %w", err)
	}
	return nil
}

func (a *App) startGatewayAfterModelRestore() error {
	if a != nil && a.modelRecoveryStart != nil {
		if err := a.modelRecoveryStart(); err != nil {
			return err
		}
	} else {
		if handled, _, err := gatewayServiceStartIfInstalled(a.configPath); handled {
			if err != nil {
				return err
			}
		} else if _, err := gatewayrt.StartDetached(gatewayrt.StartOptions{Replace: true, ConfigPath: a.configPath}); err != nil {
			return err
		}
	}
	if a != nil && a.modelRecoveryWait != nil {
		return a.modelRecoveryWait()
	}
	ctx, cancel := contextWithTimeout(a.ctx, 15*time.Second)
	defer cancel()
	if _, err := gatewayrt.WaitForRunning(ctx, gatewayrt.EnsureOptions{ConfigPath: a.configPath, Timeout: 12 * time.Second}); err != nil {
		return fmt.Errorf("start the restored model runtime: %w", err)
	}
	return nil
}

func (a *App) modelDaemonRunning() bool {
	_, ok := a.modelDaemonRecord()
	return ok
}

func (a *App) modelDaemonRecord() (gatewayrt.StatusRecord, bool) {
	if a == nil || !a.gatewayTargetIsLocal() {
		return gatewayrt.StatusRecord{}, false
	}
	record, ok := gatewayrt.NewManager(a.gatewayDataDir(), "").RunningRecord()
	if !ok {
		return gatewayrt.StatusRecord{}, false
	}
	selectedPath, _ := config.ResolveConfigPath(a.configPath)
	if strings.TrimSpace(record.ConfigPath) != "" {
		runningPath, _ := config.ResolveConfigPath(record.ConfigPath)
		if !samePath(selectedPath, runningPath) {
			return gatewayrt.StatusRecord{}, false
		}
	} else if strings.TrimSpace(a.configPath) != "" {
		// Older daemons did not record their config path. Never route an
		// explicit alternate-config command into an ambiguous owner.
		return gatewayrt.StatusRecord{}, false
	}
	return record, true
}

type modelApplyReceipt struct {
	Selection        config.ModelSelectionConfig
	ChangeID         string
	Online           bool
	RestartScheduled bool
	Notices          []string
	LegacyDaemon     bool
}

func (a *App) commitModelCandidate(cfg *config.Config, target modelchange.Route, provider, model string, reasoning, serviceTier modelchange.OptionalValue, candidate modelchange.CandidateResult) (modelApplyReceipt, error) {
	request := api.ModelChangeRequest{
		Action: "apply", Route: string(target), Provider: provider, Model: model,
		Reasoning: optionalPointer(reasoning), ServiceTier: optionalPointer(serviceTier),
	}
	if response, online, err := a.applyModelThroughDaemon(request); online {
		if err != nil {
			return modelApplyReceipt{}, err
		}
		return modelApplyReceipt{
			Selection: modelchange.SelectionForRoute(response.Change.Candidate, target),
			ChangeID:  response.Change.ID, Online: true,
			RestartScheduled: response.RestartScheduled, Notices: response.Notices,
		}, nil
	} else if err != nil {
		return modelApplyReceipt{}, err
	}
	service := &modelchange.Service{ConfigPath: cfg.Path, Validate: a.modelChangeValidator()}
	legacyDaemon := a.modelDaemonRunning()
	status, err := service.Inspect()
	if err != nil {
		return modelApplyReceipt{}, err
	}
	result, err := service.Prepare(a.ctx, modelchange.PrepareRequest{
		Candidate: candidate.Snapshot, Source: "local-cli-offline",
		ExpectedGeneration: status.Generation, RequireConfirmation: false,
		ReplacePending: status.Pending != nil && status.Pending.Source == "local-cli-offline",
	})
	if err != nil {
		return modelApplyReceipt{}, err
	}
	if _, err := service.BeginDraining(result.Change.ID); err != nil {
		return modelApplyReceipt{}, err
	}
	if _, err := service.MarkRestarting(result.Change.ID, "on-demand"); err != nil {
		return modelApplyReceipt{}, err
	}
	return modelApplyReceipt{
		Selection: modelchange.SelectionForRoute(result.Change.Candidate, target),
		ChangeID:  result.Change.ID, LegacyDaemon: legacyDaemon,
	}, nil
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
	prompt := fmt.Sprintf("API key for %s (blank keeps %s, '-' clears)", label, currentLabel)
	raw, err := a.promptSecretInput(prompt)
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

func (a *App) promptSecretInput(label string) (string, error) {
	file, terminal := a.stdin.(*os.File)
	if !terminal || !a.interactive {
		return a.promptInput(label, "")
	}
	fmt.Fprintf(a.stdout, "%s: ", label)
	disable := exec.Command("stty", "-echo")
	disable.Stdin = file
	if output, err := disable.CombinedOutput(); err != nil {
		return "", fmt.Errorf("disable terminal echo: %w: %s", err, strings.TrimSpace(string(output)))
	}
	defer func() {
		restore := exec.Command("stty", "echo")
		restore.Stdin = file
		_ = restore.Run()
	}()
	if a.input == nil {
		a.input = bufio.NewReader(a.stdin)
	}
	raw, err := a.input.ReadString('\n')
	fmt.Fprintln(a.stdout)
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimSpace(raw), nil
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
	// The model command reads current providers.<id> and legacy provider_profiles through
	// the same view so users can upgrade config.yaml gradually.
	id := modelruntime.NormalizeProviderID(provider)
	if endpoint, ok := cfg.Providers.BuiltinEndpoint(id); ok {
		return endpoint
	}
	switch id {
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
	cfg.Providers.SetBuiltinEndpoint(id, endpoint)
}

func clearProviderCredentialForModelCommand(cfg *config.Config, provider string) {
	if cfg == nil {
		return
	}
	id := modelruntime.NormalizeProviderID(provider)
	if endpoint, ok := cfg.Providers.BuiltinEndpoint(id); ok {
		endpoint.APIKey = ""
		cfg.Providers.SetBuiltinEndpoint(id, endpoint)
	}
	switch id {
	case "openai":
		cfg.Providers.OpenAI.APIKey = ""
		cfg.Providers.OpenAIAPIKey = ""
	case "anthropic":
		cfg.Providers.Anthropic.APIKey = ""
		cfg.Providers.AnthropicAPIKey = ""
	case "google", "gemini":
		cfg.Providers.Google.APIKey = ""
		cfg.Providers.GeminiAPIKey = ""
	case "openrouter":
		cfg.Providers.OpenRouterAPIKey = ""
	case "minimax":
		cfg.Providers.MiniMaxAPIKey = ""
	}
	if endpoint, ok := cfg.ProviderProfiles[id]; ok {
		endpoint.APIKey = ""
		cfg.ProviderProfiles[id] = endpoint
	}
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
