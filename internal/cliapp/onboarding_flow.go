package cliapp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	appcore "selfmind/internal/app"
	"selfmind/internal/modelruntime"
	"selfmind/internal/platform/config"
	"selfmind/internal/tools"
)

type onboardingOptions struct {
	Explicit       bool
	NonInteractive bool
	SkipModel      bool
	SkipGateway    bool
}

func (a *App) ensureOnboarding(cfg *config.Config, options onboardingOptions) (*config.Config, int) {
	statePath := onboardingStatePath(cfg, a.configPath)
	state, err := loadOnboardingState(statePath)
	if err != nil {
		fmt.Fprintf(a.stderr, "SelfMind setup state error: %v\n", err)
		return nil, 1
	}
	if !options.Explicit && state.coreReady(cfg) && a.expectedBackgroundStateReady(state) {
		a.onboarding = &state
		return cfg, 0
	}
	if !a.interactive && !options.NonInteractive {
		fmt.Fprintln(a.stderr, "SelfMind setup is incomplete.")
		fmt.Fprintln(a.stderr, "Run `selfmind setup` in an interactive terminal.")
		return nil, 1
	}

	if state.Version == 0 {
		a.printOnboardingWelcome()
	} else {
		fmt.Fprintln(a.stdout, "SelfMind setup")
		fmt.Fprintln(a.stdout)
	}

	if !options.SkipModel {
		var code int
		cfg, code = a.runOnboardingModelStep(cfg, &state, options)
		if code != 0 {
			_ = saveOnboardingState(statePath, state)
			return nil, code
		}
		if err := saveOnboardingState(statePath, state); err != nil {
			fmt.Fprintf(a.stderr, "Save setup state: %v\n", err)
			return nil, 1
		}
	}

	if !options.SkipGateway {
		if code := a.runOnboardingRuntimeStep(cfg, &state, options); code != 0 {
			_ = saveOnboardingState(statePath, state)
			return nil, code
		}
		if err := saveOnboardingState(statePath, state); err != nil {
			fmt.Fprintf(a.stderr, "Save setup state: %v\n", err)
			return nil, 1
		}
	}

	a.onboarding = &state
	if state.coreReady(cfg) {
		if options.Explicit {
			fmt.Fprintln(a.stdout, "Setup complete. Run `selfmind` to start the CLI.")
		} else {
			fmt.Fprintln(a.stdout, "Setup ready.")
			fmt.Fprintln(a.stdout, "Starting SelfMind...")
		}
		fmt.Fprintln(a.stdout)
	} else {
		fmt.Fprintln(a.stdout, "Setup is incomplete. Run `selfmind setup` to continue.")
	}
	return cfg, 0
}

func (a *App) printOnboardingWelcome() {
	fmt.Fprintln(a.stdout, "Welcome to SelfMind")
	fmt.Fprintln(a.stdout)
	fmt.Fprintln(a.stdout, "SelfMind will:")
	fmt.Fprintln(a.stdout, "  1. Connect and verify your AI models")
	fmt.Fprintln(a.stdout, "  2. Configure your workspace and safety mode")
	fmt.Fprintln(a.stdout, "  3. Enable reliable background operation")
	fmt.Fprintln(a.stdout)
	fmt.Fprintln(a.stdout, "No administrator access is required for the personal setup.")
	fmt.Fprintln(a.stdout)
}

func (a *App) runOnboardingModelStep(cfg *config.Config, state *onboardingState, options onboardingOptions) (*config.Config, int) {
	if !modelSelectionConfigured(cfg) {
		if options.NonInteractive {
			fmt.Fprintln(a.stderr, "No primary model is configured.")
			fmt.Fprintln(a.stderr, "Run `selfmind setup` interactively or configure models.primary and models.auxiliary.")
			return nil, 1
		}
		if applied, err := a.applyRecommendedReadyPrimary(cfg); err != nil {
			fmt.Fprintf(a.stderr, "Use detected model: %v\n", err)
			return nil, 1
		} else if !applied {
			if code := a.runInteractiveModelPicker(cfg); code != 0 {
				return nil, code
			}
		}
		var err error
		cfg, err = config.LoadConfig(config.Options{Path: a.configPath})
		if err != nil {
			fmt.Fprintf(a.stderr, "Reload configured model: %v\n", err)
			return nil, 1
		}
		if !modelSelectionConfigured(cfg) {
			fmt.Fprintln(a.stderr, "Setup cancelled. No primary model was configured.")
			return nil, 1
		}
	}

	cfg.InitializeAuxiliaryFromPrimary()
	a.printModelPair(cfg)
	if !options.NonInteractive {
		ok, err := a.promptConfirm("Continue with these models?", true)
		if err != nil {
			fmt.Fprintln(a.stderr, err)
			return nil, 1
		}
		if !ok {
			if code := a.customizeOnboardingModels(cfg); code != 0 {
				return nil, code
			}
			var loadErr error
			cfg, loadErr = config.LoadConfig(config.Options{Path: a.configPath})
			if loadErr != nil {
				fmt.Fprintf(a.stderr, "Reload configured models: %v\n", loadErr)
				return nil, 1
			}
			cfg.InitializeAuxiliaryFromPrimary()
			a.printModelPair(cfg)
		}
	}
	if err := a.ensurePersistentOnboardingCredentials(cfg); err != nil {
		fmt.Fprintf(a.stderr, "Credential setup failed: %v\n", err)
		return nil, 1
	}

	fmt.Fprintln(a.stdout, "Verifying models...")
	if err := a.probeOnboardingModel(cfg, "primary"); err != nil {
		fmt.Fprintf(a.stderr, "Primary model verification failed: %v\n", err)
		fmt.Fprintln(a.stderr, "Choose another model or repair its credential, then retry setup.")
		return nil, 1
	}
	auxiliaryDegraded := false
	if err := a.probeOnboardingModel(cfg, "auxiliary"); err != nil {
		fmt.Fprintf(a.stderr, "Background model verification failed: %v\n", err)
		if options.NonInteractive {
			return nil, 1
		}
		continueDegraded, promptErr := a.promptConfirm("Continue with background features limited?", false)
		if promptErr != nil || !continueDegraded {
			return nil, 1
		}
		auxiliaryDegraded = true
	}
	state.recordModels(cfg, auxiliaryDegraded)
	fmt.Fprintln(a.stdout, "  ✓ Primary model verified")
	if auxiliaryDegraded {
		fmt.Fprintln(a.stdout, "  ! Background model unavailable (degraded)")
	} else {
		fmt.Fprintln(a.stdout, "  ✓ Background model verified")
	}
	fmt.Fprintln(a.stdout)
	return cfg, 0
}

func (a *App) applyRecommendedReadyPrimary(cfg *config.Config) (bool, error) {
	resolver := modelruntime.NewResolver(cfg)
	choicesByID := map[string]modelChoice{}
	for _, choice := range a.modelChoices(cfg) {
		choicesByID[choice.ID] = choice
	}
	priority := []string{
		"codex-cli", "claude-code", "gemini-cli", "qwen-cli",
		"openai", "anthropic", "google", "minimax-oauth", "minimax",
		"kimi-coding", "openrouter", "deepseek", "zai", "alibaba-coding-plan",
	}
	for _, providerID := range priority {
		choice := choicesByID[providerID]
		if !choice.Ready || choice.Kind != "builtin" {
			continue
		}
		profile, ok := resolver.Registry().Resolve(choice.ID)
		if !ok {
			continue
		}
		runtime, err := resolver.Resolve(a.ctx, modelruntime.Selection{
			Provider: profile.ID, Model: firstModelChoice(profile.FallbackModels),
		})
		if err != nil || strings.TrimSpace(runtime.Model) == "" {
			continue
		}
		endpoint := providerEndpointForModelCommand(cfg, profile.ID)
		endpoint.BaseURL = firstNonEmpty(endpoint.BaseURL, runtime.BaseURL, profile.BaseURL)
		endpoint.Protocol = modelruntime.NormalizeProtocol(firstNonEmpty(endpoint.Protocol, runtime.Protocol, profile.Protocol))
		endpoint.Model = ""
		if profile.AuthType != modelruntime.AuthExternalOAuth && profile.AuthType != modelruntime.AuthMiniMaxOAuth {
			if err := modelruntime.NewCredentialStore(cfg.Auth.CredentialsFile).SaveAPIKey(profile.ID, runtime.APIKey); err != nil {
				return false, err
			}
		}
		endpoint.APIKey = ""
		setProviderEndpointForModelCommand(cfg, profile.ID, endpoint)
		if profile.AuthType != modelruntime.AuthExternalOAuth && profile.AuthType != modelruntime.AuthMiniMaxOAuth {
			clearProviderCredentialForModelCommand(cfg, profile.ID)
		}
		cfg.SetPrimaryModel(profile.ID, runtime.Model, "")
		if err := config.SaveConfig(cfg.Path, cfg); err != nil {
			return false, err
		}
		fmt.Fprintf(a.stdout, "Detected %s credentials from %s.\n\n", profile.DisplayName, runtime.CredentialSource)
		return true, nil
	}
	return false, nil
}

func (a *App) ensurePersistentOnboardingCredentials(cfg *config.Config) error {
	seen := map[string]bool{}
	for _, role := range []string{"primary", "auxiliary"} {
		runtime, err := appcore.ResolveModelRuntime(a.ctx, cfg, role)
		if err != nil {
			continue // the live probe below owns the actionable resolution failure
		}
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(runtime.CredentialSource)), "env:") || strings.TrimSpace(runtime.APIKey) == "" {
			continue
		}
		provider := strings.TrimSpace(runtime.Provider)
		if seen[strings.ToLower(provider)] {
			continue
		}
		seen[strings.ToLower(provider)] = true
		if err := modelruntime.NewCredentialStore(cfg.Auth.CredentialsFile).SaveAPIKey(provider, runtime.APIKey); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) printModelPair(cfg *config.Config) {
	primary := cfg.EffectivePrimary()
	auxiliary := cfg.EffectiveAuxiliary()
	fmt.Fprintln(a.stdout, "Model setup")
	fmt.Fprintln(a.stdout)
	fmt.Fprintf(a.stdout, "  Primary:    %s / %s\n", blankAsDash(primary.Provider), blankAsDash(primary.Model))
	fmt.Fprintln(a.stdout, "              Conversations, planning, tools, and final answers")
	fmt.Fprintf(a.stdout, "  Background: %s / %s\n", blankAsDash(auxiliary.Provider), blankAsDash(auxiliary.Model))
	fmt.Fprintln(a.stdout, "              Memory, summaries, reviews, and maintenance")
	fmt.Fprintln(a.stdout, "  Credentials needed by background work stay in SelfMind's private auth store.")
	fmt.Fprintln(a.stdout)
}

func (a *App) customizeOnboardingModels(cfg *config.Config) int {
	changePrimary, err := a.promptConfirm("Change the primary model?", false)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	if changePrimary {
		if code := a.runInteractiveModelPickerFor(cfg, "primary"); code != 0 {
			return code
		}
		var loadErr error
		cfg, loadErr = config.LoadConfig(config.Options{Path: a.configPath})
		if loadErr != nil {
			fmt.Fprintln(a.stderr, loadErr)
			return 1
		}
	}
	changeAuxiliary, err := a.promptConfirm("Change the background model?", false)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	if changeAuxiliary {
		return a.runInteractiveModelPickerFor(cfg, "auxiliary")
	}
	return 0
}

func (a *App) probeOnboardingModel(cfg *config.Config, role string) error {
	if a.onboardingProbe != nil {
		return a.onboardingProbe(cfg, role)
	}
	runtime, err := appcore.ResolveModelRuntime(a.ctx, cfg, role)
	if err != nil {
		return err
	}
	probe := appcore.ProbeResolvedModelForRole(a.ctx, runtime, role)
	if probe.Err != nil {
		return fmt.Errorf("%s", tools.RedactSensitive(probe.Err.Error()))
	}
	return nil
}

func (a *App) promptConfirm(label string, defaultYes bool) (bool, error) {
	defaultValue := "y"
	suffix := "[Y/n]"
	if !defaultYes {
		defaultValue = "n"
		suffix = "[y/N]"
	}
	for {
		value, err := a.promptInput(label+" "+suffix, defaultValue)
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		default:
			fmt.Fprintln(a.stdout, "Enter y or n.")
		}
	}
}

func canonicalOnboardingWorkspace(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("workspace path is required")
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace is not a directory: %s", resolved)
	}
	return filepath.Clean(resolved), nil
}

func onboardingWorkspaceNeedsExplicitChoice(path string) bool {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "" || path == string(filepath.Separator) {
		return true
	}
	home, err := os.UserHomeDir()
	return err == nil && filepath.Clean(home) == path
}

func onboardingModelLabel(selection config.ModelSelectionConfig) string {
	if strings.TrimSpace(selection.Provider) == "" && strings.TrimSpace(selection.Model) == "" {
		return "not configured"
	}
	return strings.TrimSpace(selection.Provider) + "/" + strings.TrimSpace(selection.Model)
}

func onboardingStateAge(state onboardingState) time.Duration {
	if state.UpdatedAt.IsZero() {
		return 0
	}
	return time.Since(state.UpdatedAt)
}
