package cliapp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"selfmind/internal/modelchange"
	"selfmind/internal/platform/config"
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
	modelsReady, err := a.modelReadiness(cfg, &state)
	if err != nil {
		fmt.Fprintf(a.stderr, "Model readiness error: %v\n", err)
		return nil, 1
	}
	if modelsReady && state.Version > 0 && state.Version < onboardingStateVersion {
		if err := saveOnboardingState(statePath, state); err != nil {
			fmt.Fprintf(a.stderr, "Migrate setup state: %v\n", err)
			return nil, 1
		}
		state.retireLegacyModels()
	}
	runtimeReady := state.runtimeReady() && a.expectedBackgroundStateReady(state)
	if !options.Explicit && modelsReady && runtimeReady {
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

	if !modelsReady && !options.SkipModel {
		if options.Explicit || options.NonInteractive || !a.interactive {
			fmt.Fprintln(a.stderr, "Model Readiness is missing. Run `selfmind model` in an interactive terminal.")
			return nil, 1
		}
		fmt.Fprintln(a.stdout, "Model Readiness is required. Opening Model Manager...")
		fmt.Fprintln(a.stdout)
		a.modelManagerOnly = true
		return cfg, 0
	} else if modelsReady && !runtimeReady {
		fmt.Fprintln(a.stdout, "  ✓ Models ready")
		fmt.Fprintln(a.stdout)
	}

	if !runtimeReady && !options.SkipGateway {
		runtimeOptions := options
		runtimeOptions.SkipModel = true
		if code := a.runOnboardingRuntimeStep(&state, runtimeOptions); code != 0 {
			_ = saveOnboardingState(statePath, state)
			return nil, code
		}
		if err := saveOnboardingState(statePath, state); err != nil {
			fmt.Fprintf(a.stderr, "Save setup state: %v\n", err)
			return nil, 1
		}
		runtimeReady = state.runtimeReady() && a.expectedBackgroundStateReady(state)
	}

	a.onboarding = &state
	if modelsReady && runtimeReady {
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

func (a *App) modelReadiness(cfg *config.Config, state *onboardingState) (bool, error) {
	models := &modelchange.Service{ConfigPath: cfg.Path}
	status, err := models.Inspect()
	if err != nil {
		return false, err
	}
	if status.ModelReady() {
		return true, nil
	}
	// A schema-v1 onboarding receipt is accepted once as migration evidence.
	// Subsequent writes use model-state as the sole readiness authority.
	if state != nil && state.matchesModels(cfg) {
		status, err = models.AcceptMigrationReadiness()
		if err != nil {
			return false, err
		}
		return status.ModelReady(), nil
	}
	return false, nil
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
