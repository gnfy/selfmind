package cliapp

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	gatewayrt "selfmind/internal/runtime/gateway"
)

func (a *App) runUninstallCommandIfRequested() (bool, int) {
	if len(a.args) < 2 || a.args[1] != "uninstall" {
		return false, 0
	}
	fs := flag.NewFlagSet("selfmind uninstall", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	prepare := fs.Bool("prepare", false, "gracefully stop the daemon and preserve user data")
	purgeData := fs.Bool("purge-data", false, "delete ~/.selfmind after stopping the daemon")
	yes := fs.Bool("yes", false, "confirm destructive data deletion")
	if err := fs.Parse(a.args[2:]); err != nil {
		return true, 2
	}
	if !*prepare && !*purgeData {
		fmt.Fprintln(a.stderr, "usage: selfmind uninstall --prepare [--purge-data --yes]")
		return true, 2
	}

	timeout := gatewayrt.ResolveDrainTimeout() + 10*time.Second
	ctx, cancel := contextWithTimeout(a.ctx, timeout)
	defer cancel()
	if err := gatewayrt.RequestShutdown(ctx, gatewayrt.StopOptions{
		URL:     a.gatewayURL(),
		DataDir: a.gatewayDataDir(),
		Timeout: timeout,
	}); err != nil {
		fmt.Fprintf(a.stderr, "Could not stop the gateway cleanly: %v\n", err)
		return true, 1
	}
	fmt.Fprintln(a.stdout, "SelfMind gateway stopped.")

	dataDir, err := selfMindHome()
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return true, 1
	}
	if *purgeData {
		if !*yes {
			fmt.Fprintln(a.stderr, "`--purge-data` requires `--yes`.")
			return true, 2
		}
		if err := validateSelfMindHome(dataDir); err != nil {
			fmt.Fprintf(a.stderr, "Refusing to delete data: %v\n", err)
			return true, 1
		}
		if err := os.RemoveAll(dataDir); err != nil {
			fmt.Fprintf(a.stderr, "Delete %s: %v\n", dataDir, err)
			return true, 1
		}
		fmt.Fprintf(a.stdout, "Deleted SelfMind data: %s\n", dataDir)
	} else {
		fmt.Fprintf(a.stdout, "Preserved config and data: %s\n", dataDir)
	}
	fmt.Fprintf(a.stdout, "Remove the npm package with: %s\n", uninstallPackageCommand())
	return true, 0
}

func selfMindHome() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".selfmind"), nil
}

func validateSelfMindHome(path string) error {
	clean := filepath.Clean(strings.TrimSpace(path))
	if clean == "" || clean == "." || clean == string(filepath.Separator) {
		return fmt.Errorf("unsafe path %q", path)
	}
	if filepath.Base(clean) != ".selfmind" {
		return fmt.Errorf("expected a .selfmind directory, got %q", clean)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if filepath.Clean(filepath.Dir(clean)) != filepath.Clean(home) {
		return fmt.Errorf("%q is not directly under the user home", clean)
	}
	return nil
}

func uninstallPackageCommand() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SELFMIND_INSTALL_METHOD"))) {
	case "pnpm":
		return "pnpm remove -g selfmind"
	case "yarn":
		return "yarn global remove selfmind"
	case "bun":
		return "bun remove -g selfmind"
	default:
		return "npm uninstall -g selfmind"
	}
}
