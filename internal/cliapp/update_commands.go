package cliapp

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"selfmind/internal/buildinfo"
	"selfmind/internal/platform/config"
	gatewayrt "selfmind/internal/runtime/gateway"
	"selfmind/internal/updatecheck"
)

func (a *App) runUpdateCommandIfRequested() (bool, int) {
	if len(a.args) < 2 || a.args[1] != "update" {
		return false, 0
	}
	args := a.args[2:]
	if len(args) > 0 && args[0] == "check" {
		return true, a.updateCheck(args[1:])
	}
	return true, a.updateApply(args)
}

// updateCheck is the read-only path: query the npm dist-tag and report,
// never install.
func (a *App) updateCheck(args []string) int {
	fs := flag.NewFlagSet("selfmind update check", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	channel := fs.String("channel", "", "npm dist-tag to check: latest or next")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	selectedChannel, _, code := a.updateChannel(*channel)
	if code != 0 {
		return code
	}
	ctx, cancel := context.WithTimeout(a.ctx, 8*time.Second)
	defer cancel()
	result, err := updatecheck.Check(ctx, buildinfo.Version, selectedChannel)
	if err != nil {
		fmt.Fprintf(a.stderr, "Update check failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(a.stdout, "Current: %s\nAvailable (%s): %s\n", result.Current, result.Channel, result.Latest)
	if result.UpdateAvailable() {
		fmt.Fprintf(a.stdout, "Update with: %s\n", updateInstallCommand(result.Channel))
	} else {
		fmt.Fprintln(a.stdout, "SelfMind is up to date.")
	}
	return 0
}

// updateApply performs the full upgrade: check, install through the user's
// package manager, verify the new binary, refresh the notice cache, and
// restart the gateway daemon (drain-by-default) so it runs the new version.
func (a *App) updateApply(args []string) int {
	fs := flag.NewFlagSet("selfmind update", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	channel := fs.String("channel", "", "npm dist-tag to install: latest or next")
	force := fs.Bool("force", false, "reinstall even when already up to date or the check fails")
	noRestart := fs.Bool("no-restart", false, "skip the gateway daemon restart after installing")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if isDevelopmentVersion(buildinfo.Version) && !*force {
		fmt.Fprintln(a.stderr, "This is a development build; use `selfmind update --force` to overwrite it with the npm release.")
		return 1
	}
	selectedChannel, pinnedChannel, code := a.updateChannel(*channel)
	if code != 0 {
		return code
	}

	checkCtx, cancelCheck := context.WithTimeout(a.ctx, 8*time.Second)
	result, err := updatecheck.Check(checkCtx, buildinfo.Version, selectedChannel)
	cancelCheck()
	if err != nil {
		if !*force {
			fmt.Fprintf(a.stderr, "Update check failed: %v\nUse `selfmind update --force` to install anyway.\n", err)
			return 1
		}
		fmt.Fprintf(a.stderr, "Update check failed: %v — continuing because --force was set.\n", err)
	} else {
		fmt.Fprintf(a.stdout, "Current: %s\nAvailable (%s): %s\n", result.Current, result.Channel, result.Latest)
		if !result.UpdateAvailable() && !*force {
			fmt.Fprintln(a.stdout, "SelfMind is already up to date.")
			return 0
		}
	}

	installArgs := updateInstallArgs(selectedChannel, os.Getenv("SELFMIND_INSTALL_METHOD"))
	fmt.Fprintf(a.stdout, "Running: %s\n", strings.Join(installArgs, " "))
	install := exec.CommandContext(a.ctx, installArgs[0], installArgs[1:]...)
	install.Stdout = a.stdout
	install.Stderr = a.stderr
	if err := install.Run(); err != nil {
		fmt.Fprintf(a.stderr, "Install failed: %v\nNothing was restarted; the previous version keeps running.\n", err)
		return 1
	}

	// Ask the freshly installed launcher for its version. The current process
	// is still the old binary, so this is the only honest signal that the
	// install actually took effect on PATH.
	newVersion := a.installedBinaryVersion()
	if newVersion == "" {
		fmt.Fprintln(a.stderr, "Installed, but could not verify the new binary version (`selfmind --version`). Run `selfmind doctor` if anything looks wrong.")
		_ = updatecheck.InvalidateCache()
	} else {
		fmt.Fprintf(a.stdout, "Installed: SelfMind %s\n", newVersion)
		// Re-stamp the notice cache as the NEW version so a startup that
		// races the next background check never re-announces this update.
		refreshCtx, cancelRefresh := context.WithTimeout(a.ctx, 8*time.Second)
		if _, err := updatecheck.Check(refreshCtx, newVersion, selectedChannel); err != nil {
			_ = updatecheck.InvalidateCache()
		}
		cancelRefresh()
	}
	// A pinned channel is the user's explicit word — never rewrite it
	// silently. The installed binary follows its own line via channel "auto",
	// so a hint is all that is needed for a durable switch.
	if hint := channelPinHint(*channel, pinnedChannel, selectedChannel); hint != "" {
		fmt.Fprintln(a.stdout, hint)
	}

	if *noRestart {
		fmt.Fprintln(a.stdout, "Skipped daemon restart (--no-restart). The gateway keeps running the previous version until it is restarted.")
		return 0
	}
	if !a.gatewayAppearsRunning() {
		fmt.Fprintln(a.stdout, "Gateway daemon is not running; the new version will be used on its next start.")
		return 0
	}
	fmt.Fprintln(a.stdout, "Restarting the gateway daemon (waits for a safe turn boundary)...")
	if code := a.gatewayRestart(nil); code != 0 {
		fmt.Fprintln(a.stderr, "Daemon restart failed. The new version is installed but the running daemon is still the old one; run `selfmind gateway restart` manually.")
		return 1
	}
	return 0
}

// updateChannel resolves the effective dist-tag (flag > config pin > version
// inference) and reports the config pin ("" when the config follows "auto")
// so callers can warn when an explicit pin and an explicit flag disagree.
func (a *App) updateChannel(flagValue string) (effective, pinned string, code int) {
	cfg, err := config.LoadConfig(config.Options{Path: a.configPath, CreateIfMissing: true})
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return "", "", 1
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Updates.Channel)) {
	case "next", "beta":
		pinned = "next"
	case "latest":
		pinned = "latest"
	}
	selected := strings.TrimSpace(flagValue)
	if selected == "" {
		selected = cfg.Updates.Channel
	}
	return updatecheck.ResolveChannel(selected, buildinfo.Version), pinned, 0
}

// channelPinHint explains a one-shot --channel that disagrees with the
// config pin. Empty when nothing needs saying.
func channelPinHint(flagValue, pinned, effective string) string {
	if strings.TrimSpace(flagValue) == "" || pinned == "" || pinned == effective {
		return ""
	}
	return fmt.Sprintf("Note: config pins updates.channel: %s, but this install used %s. Edit ~/.selfmind/config.yaml to set updates.channel: %s for a durable switch, or \"auto\" to always follow the installed version.", pinned, effective, effective)
}

// gatewayAppearsRunning is a quick liveness probe: HTTP status first, on-disk
// running record as fallback. Update must not surprise-start a stopped daemon.
func (a *App) gatewayAppearsRunning() bool {
	ctx, cancel := contextWithTimeout(a.ctx, 2*time.Second)
	defer cancel()
	if _, status, err := gatewayrt.RequestStatus(ctx, a.gatewayURL()); err == nil && status < 400 {
		return true
	}
	_, ok := gatewayrt.NewManager(a.gatewayDataDir(), "").RunningRecord()
	return ok
}

var updateVersionPattern = regexp.MustCompile(`v?[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.]+)?`)

// installedBinaryVersion resolves `selfmind` on PATH (the npm launcher shim,
// not this process's executable) and extracts the semver from its --version
// banner. Empty means the verify step could not run or parse.
func (a *App) installedBinaryVersion() string {
	path, err := exec.LookPath("selfmind")
	if err != nil {
		return ""
	}
	ctx, cancel := contextWithTimeout(a.ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return ""
	}
	return updateVersionPattern.FindString(string(out))
}

func (a *App) printUpdateNotice(cfg *config.Config) {
	if cfg == nil || !cfg.Updates.Enabled || isDevelopmentVersion(buildinfo.Version) {
		return
	}
	interval := updatecheck.ParseInterval(cfg.Updates.CheckInterval)
	cachePath := updatecheck.CachePath()
	effectiveChannel := updatecheck.ResolveChannel(cfg.Updates.Channel, buildinfo.Version)
	cached, err := updatecheck.ReadCache(cachePath)
	if err == nil && shouldAnnounceUpdate(cached, buildinfo.Version, effectiveChannel) {
		fmt.Fprintf(a.stderr, "Update available: SelfMind %s. Run `%s`.\n", cached.Latest, updateInstallCommand(cached.Channel))
	}
	// The cache is only fresh for the binary and channel that wrote it: after
	// an upgrade the running version no longer matches, and after a channel
	// switch the cached row watches the wrong dist-tag — either way re-check
	// now instead of serving the stale row for the rest of the interval.
	if err == nil && updatecheck.Fresh(cached, interval) &&
		updatecheck.Compare(cached.Current, buildinfo.Version) == 0 &&
		cached.Channel == effectiveChannel {
		return
	}
	channel := effectiveChannel
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_, _ = updatecheck.Check(ctx, buildinfo.Version, channel)
	}()
}

// shouldAnnounceUpdate compares the cached latest against the version that is
// RUNNING NOW, never against the version recorded when the cache was written.
// After an upgrade the old cache row (current=old, latest=new) survives until
// the next check interval; comparing against the stale row kept announcing an
// update that was already installed. A row recorded for another channel never
// announces: after a channel switch it advertises the wrong version line.
func shouldAnnounceUpdate(cached updatecheck.Result, runningVersion, effectiveChannel string) bool {
	if strings.TrimSpace(cached.Latest) == "" {
		return false
	}
	if cached.Channel != effectiveChannel {
		return false
	}
	return updatecheck.Compare(cached.Latest, runningVersion) > 0
}

// updateInstallArgs builds the package-manager argv for the selected channel.
// The method comes from SELFMIND_INSTALL_METHOD (stamped by the installer);
// unknown or empty values fall back to npm.
func updateInstallArgs(channel, method string) []string {
	tag := "latest"
	if strings.EqualFold(strings.TrimSpace(channel), "next") || strings.EqualFold(strings.TrimSpace(channel), "beta") {
		tag = "next"
	}
	pkg := "@selfmind/cli@" + tag
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "pnpm":
		return []string{"pnpm", "add", "-g", pkg}
	case "yarn":
		return []string{"yarn", "global", "add", pkg}
	case "bun":
		return []string{"bun", "add", "-g", pkg}
	default:
		return []string{"npm", "install", "-g", pkg}
	}
}

func updateInstallCommand(channel string) string {
	return strings.Join(updateInstallArgs(channel, os.Getenv("SELFMIND_INSTALL_METHOD")), " ")
}

func isDevelopmentVersion(version string) bool {
	version = strings.ToLower(strings.TrimSpace(version))
	return version == "" || strings.Contains(version, "dev")
}
