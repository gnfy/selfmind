package cliapp

import (
	"flag"
	"fmt"

	"selfmind/internal/platform/config"
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
