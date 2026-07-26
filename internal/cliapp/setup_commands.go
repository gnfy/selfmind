package cliapp

import (
	"flag"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/modelruntime"
	"selfmind/internal/platform/config"
	gatewayrt "selfmind/internal/runtime/gateway"
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
	checkModel := fs.Bool("check-model", false, "resolve and validate the configured model")
	if err := fs.Parse(a.args[2:]); err != nil {
		return true, 2
	}

	cfg, err := config.LoadConfig(config.Options{Path: a.configPath, CreateIfMissing: true})
	if err != nil {
		fmt.Fprintf(a.stderr, "Setup failed: %v\n", err)
		return true, 1
	}
	fmt.Fprintf(a.stdout, "Config: %s\n", cfg.Path)

	if !*skipModel && !modelSelectionConfigured(cfg) {
		if *nonInteractive {
			fmt.Fprintln(a.stderr, "No default model is configured.")
			fmt.Fprintln(a.stderr, "Run `selfmind model`, or edit the config before running non-interactive setup.")
			return true, 1
		}
		if code := a.runInteractiveModelPicker(cfg); code != 0 {
			return true, code
		}
		cfg, err = config.LoadConfig(config.Options{Path: a.configPath})
		if err != nil {
			fmt.Fprintf(a.stderr, "Reload configured model: %v\n", err)
			return true, 1
		}
		if !modelSelectionConfigured(cfg) {
			fmt.Fprintln(a.stderr, "Setup cancelled. No default model was configured.")
			fmt.Fprintln(a.stderr, "Run `selfmind setup` when you are ready.")
			return true, 1
		}
	}
	if !*skipModel {
		fmt.Fprintf(a.stdout, "Model: %s/%s\n", blankAsDash(cfg.EffectiveProvider()), blankAsDash(cfg.EffectiveModel()))
	}
	if *checkModel && !*skipModel {
		if code := a.checkCurrentModel(cfg); code != 0 {
			return true, code
		}
	}

	if !*skipGateway {
		if !a.gatewayTargetIsLocal() {
			fmt.Fprintf(a.stdout, "Gateway: remote (%s); no local daemon was started.\n", a.gatewayURL())
		} else {
			if gatewayServiceSupported() {
				path, err := gatewayServiceInstall(a.configPath)
				if err != nil {
					fmt.Fprintf(a.stderr, "Gateway service setup failed: %v\n", err)
					fmt.Fprintln(a.stderr, "Run `selfmind gateway run` for foreground logs, or `selfmind doctor`.")
					return true, 1
				}
				fmt.Fprintf(a.stdout, "Gateway: launchd service installed and started.\nPlist: %s\n", path)
				fmt.Fprintln(a.stdout, "Setup complete. Run `selfmind` to start the CLI.")
				return true, 0
			}
			ctx, cancel := contextWithTimeout(a.ctx, 15*time.Second)
			defer cancel()
			result, err := gatewayrt.EnsureRunning(ctx, gatewayrt.EnsureOptions{
				ConfigPath: a.configPath,
				Timeout:    12 * time.Second,
			})
			if err != nil {
				fmt.Fprintf(a.stderr, "Gateway setup failed: %v\n", err)
				fmt.Fprintln(a.stderr, "Run `selfmind gateway run` for foreground logs, or `selfmind doctor`.")
				return true, 1
			}
			state := "already running"
			if result.Started {
				state = "started"
			}
			fmt.Fprintf(a.stdout, "Gateway: %s at %s\n", state, result.URL)
		}
	}

	fmt.Fprintln(a.stdout, "Setup complete. Run `selfmind` to start the CLI.")
	return true, 0
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
		fmt.Fprintln(a.stderr, "Run `selfmind setup` in an interactive terminal, or `selfmind model set <provider> <model>` for automated setup.")
		return nil, 1
	}

	fmt.Fprintln(a.stdout, "Welcome to SelfMind.")
	fmt.Fprintln(a.stdout, "Before we start, choose the AI model SelfMind should use.")
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

	fmt.Fprintf(a.stdout, "\nSetup complete: %s/%s\n", reloaded.EffectiveProvider(), reloaded.EffectiveModel())
	fmt.Fprintln(a.stdout, "Starting SelfMind...")
	fmt.Fprintln(a.stdout)
	return reloaded, 0
}
