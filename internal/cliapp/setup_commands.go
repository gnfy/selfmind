package cliapp

import (
	"flag"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/platform/config"
	gatewayrt "selfmind/internal/runtime/gateway"
)

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
