package cliapp

import (
	"flag"
	"fmt"
	"strings"

	"selfmind/internal/modelruntime"
	"selfmind/internal/platform/config"
)

type modelSetupFunc func(*config.Config) int

func (a *App) runSetupCommandIfRequested() (bool, int) {
	if len(a.args) < 2 || a.args[1] != "setup" {
		return false, 0
	}
	fs := flag.NewFlagSet("selfmind setup", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	nonInteractive := fs.Bool("non-interactive", false, "do not prompt for missing model settings")
	skipModel := fs.Bool("skip-model", false, "do not configure or validate a model")
	skipGateway := fs.Bool("skip-gateway", false, "do not start the local gateway")
	checkModel := fs.Bool("check-model", false, "compatibility flag; setup always validates configured models live")
	if err := fs.Parse(a.args[2:]); err != nil {
		return true, 2
	}

	cfg, err := config.LoadConfig(config.Options{Path: a.configPath, CreateIfMissing: true})
	if err != nil {
		fmt.Fprintf(a.stderr, "Setup failed: %v\n", err)
		return true, 1
	}
	fmt.Fprintf(a.stdout, "Config: %s\n", cfg.Path)
	_ = checkModel
	_, code := a.ensureOnboarding(cfg, onboardingOptions{
		Explicit: true, NonInteractive: *nonInteractive,
		SkipModel: *skipModel, SkipGateway: *skipGateway,
	})
	return true, code
}

func modelSelectionConfigured(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	return strings.TrimSpace(cfg.EffectiveProvider()) != "" && strings.TrimSpace(cfg.EffectiveModel()) != ""
}

func (a *App) ensureInitialModelSetup(cfg *config.Config, setup modelSetupFunc) (*config.Config, int) {
	if modelSelectionConfigured(cfg) {
		return cfg, 0
	}
	if !a.interactive {
		fmt.Fprintln(a.stderr, "SelfMind is not configured with an AI model.")
		fmt.Fprintln(a.stderr, "Run `selfmind setup` or `selfmind model` in an interactive terminal. For automation, configure `models.primary` in YAML.")
		return nil, 1
	}

	fmt.Fprintln(a.stdout, "Welcome to SelfMind.")
	fmt.Fprintln(a.stdout, "Before we start, choose the primary model for conversations and task execution.")
	fmt.Fprintln(a.stdout, "Background roles initially reuse that model through the auxiliary slot; you can customize it later.")
	fmt.Fprintln(a.stdout, "You can reuse an existing Codex, Claude Code, Gemini, or Qwen login, or configure an API key.")
	fmt.Fprintln(a.stdout)

	if setup == nil {
		setup = a.runInteractiveModelPicker
	}
	if code := setup(cfg); code != 0 {
		return nil, code
	}

	reloaded, err := config.LoadConfig(config.Options{Path: a.configPath})
	if err != nil {
		fmt.Fprintf(a.stderr, "Reload configured model: %v\n", err)
		return nil, 1
	}
	if !modelSelectionConfigured(reloaded) {
		fmt.Fprintln(a.stderr, "Setup cancelled. No default model was configured.")
		fmt.Fprintln(a.stderr, "Run `selfmind setup` when you are ready.")
		return nil, 1
	}
	if _, err := modelruntime.NewResolver(reloaded).Resolve(a.ctx, modelruntime.Selection{}); err != nil {
		fmt.Fprintf(a.stderr, "The selected model is not ready: %v\n", err)
		fmt.Fprintln(a.stderr, "Run `selfmind setup` to choose another provider, or `selfmind doctor` for details.")
		return nil, 1
	}

	reloaded = a.ensureBackgroundRoleSetup(reloaded)

	fmt.Fprintf(a.stdout, "\nSetup complete: %s/%s\n", reloaded.EffectiveProvider(), reloaded.EffectiveModel())
	fmt.Fprintln(a.stdout, "Starting SelfMind...")
	fmt.Fprintln(a.stdout)
	return reloaded, 0
}
