package cliapp

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"selfmind/internal/buildinfo"
	"selfmind/internal/platform/config"
	"selfmind/internal/updatecheck"
)

func (a *App) runUpdateCommandIfRequested() (bool, int) {
	if len(a.args) < 2 || a.args[1] != "update" {
		return false, 0
	}
	args := a.args[2:]
	if len(args) > 0 && args[0] == "check" {
		args = args[1:]
	}
	fs := flag.NewFlagSet("selfmind update", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	channel := fs.String("channel", "", "npm dist-tag to check: latest or next")
	if err := fs.Parse(args); err != nil {
		return true, 2
	}
	cfg, err := config.LoadConfig(config.Options{Path: a.configPath, CreateIfMissing: true})
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return true, 1
	}
	selectedChannel := strings.TrimSpace(*channel)
	if selectedChannel == "" {
		selectedChannel = cfg.Updates.Channel
	}
	ctx, cancel := context.WithTimeout(a.ctx, 8*time.Second)
	defer cancel()
	result, err := updatecheck.Check(ctx, buildinfo.Version, selectedChannel)
	if err != nil {
		fmt.Fprintf(a.stderr, "Update check failed: %v\n", err)
		return true, 1
	}
	fmt.Fprintf(a.stdout, "Current: %s\nAvailable (%s): %s\n", result.Current, result.Channel, result.Latest)
	if result.UpdateAvailable() {
		fmt.Fprintf(a.stdout, "Update with: %s\n", updateInstallCommand(result.Channel))
	} else {
		fmt.Fprintln(a.stdout, "SelfMind is up to date.")
	}
	return true, 0
}

func (a *App) printUpdateNotice(cfg *config.Config) {
	if cfg == nil || !cfg.Updates.Enabled || isDevelopmentVersion(buildinfo.Version) {
		return
	}
	interval := updatecheck.ParseInterval(cfg.Updates.CheckInterval)
	cachePath := updatecheck.CachePath()
	cached, err := updatecheck.ReadCache(cachePath)
	if err == nil && cached.UpdateAvailable() {
		fmt.Fprintf(a.stderr, "Update available: SelfMind %s. Run `%s`.\n", cached.Latest, updateInstallCommand(cached.Channel))
	}
	if err == nil && updatecheck.Fresh(cached, interval) {
		return
	}
	channel := cfg.Updates.Channel
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_, _ = updatecheck.Check(ctx, buildinfo.Version, channel)
	}()
}

func updateInstallCommand(channel string) string {
	tag := "latest"
	if strings.EqualFold(strings.TrimSpace(channel), "next") || strings.EqualFold(strings.TrimSpace(channel), "beta") {
		tag = "next"
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SELFMIND_INSTALL_METHOD"))) {
	case "pnpm":
		return "pnpm add -g selfmind@" + tag
	case "yarn":
		return "yarn global add selfmind@" + tag
	case "bun":
		return "bun add -g selfmind@" + tag
	default:
		return "npm install -g selfmind@" + tag
	}
}

func isDevelopmentVersion(version string) bool {
	version = strings.ToLower(strings.TrimSpace(version))
	return version == "" || strings.Contains(version, "dev")
}
