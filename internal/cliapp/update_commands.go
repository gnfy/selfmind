package cliapp

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"selfmind/internal/buildinfo"
	"selfmind/internal/gateway/api"
	tui "selfmind/internal/gateway/cli"
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
	ctx, cancel := context.WithTimeout(a.ctx, updatecheck.RequestTimeout)
	defer cancel()
	result, err := updatecheck.Check(ctx, buildinfo.Version, selectedChannel)
	if err != nil {
		fmt.Fprintf(a.stderr, "Update check failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(a.stdout, "Current: %s\nAvailable (%s): %s\n", result.Current, result.Channel, result.Latest)
	if result.UpdateAvailable() {
		fmt.Fprintln(a.stdout, updatecheck.AvailableNotice(result.Latest))
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
	force := fs.Bool("force", false, "install even when the check fails or this would replace a newer local build")
	noRestart := fs.Bool("no-restart", false, "skip the gateway daemon restart after installing")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if isDevelopmentVersion(buildinfo.Version) && !*force && !runningFromNPMInstall() {
		fmt.Fprintln(a.stderr, "This is a development build; use `selfmind update --force` to overwrite it with the npm release.")
		return 1
	}
	selectedChannel, pinnedChannel, code := a.updateChannel(*channel)
	if code != 0 {
		return code
	}

	checkCtx, cancelCheck := context.WithTimeout(a.ctx, updatecheck.RequestTimeout)
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
		disposition := chooseUpdateDisposition(result.Current, result.Latest, *force)
		if disposition == updateSkipNewer {
			fmt.Fprintf(a.stdout, "SelfMind %s is newer than the %s channel (%s); nothing was installed. Use `selfmind update --force` to replace it with that channel.\n", result.Current, result.Channel, result.Latest)
			return 0
		}
		if disposition == updateRefresh {
			fmt.Fprintln(a.stdout, "Refreshing the current npm release so locally replaced package files are restored.")
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
		refreshCtx, cancelRefresh := context.WithTimeout(a.ctx, updatecheck.RequestTimeout)
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
	if err := a.restartDaemonViaInstalledLauncher(); err != nil {
		fmt.Fprintf(a.stderr, "Daemon restart failed: %v\nThe new version is installed but the running daemon is still the old one; run `selfmind gateway restart` manually.\n", err)
		return 1
	}
	a.verifyRestartedDaemonVersion(newVersion)
	return 0
}

// restartDaemonViaInstalledLauncher restarts the gateway through the FRESHLY
// INSTALLED launcher on PATH instead of this process. After the package
// upgrade above, this process's own executable path may already be gone — npm
// swaps the global package directory (rename to a .cli-<rand> staging dir,
// then delete), so an in-process restart fork/execs a deleted path (observed
// live). The new launcher restarts itself with a valid path, and the daemon
// it spawns is guaranteed to be the new version.
func (a *App) restartDaemonViaInstalledLauncher() error {
	path, err := exec.LookPath("selfmind")
	if err != nil {
		return fmt.Errorf("locate the installed `selfmind` launcher on PATH: %w", err)
	}
	var args []string
	if a.configPath != "" {
		args = append(args, "--config", a.configPath)
	}
	args = append(args, "gateway", "restart")
	ctx, cancel := contextWithTimeout(a.ctx, gatewayrt.ResolveDrainTimeout()+30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdout = a.stdout
	cmd.Stderr = a.stderr
	return cmd.Run()
}

// verifyRestartedDaemonVersion polls the restarted daemon's status endpoint
// and confirms it now reports the freshly installed version (codex's daemon
// updater applies the same restart-then-verify discipline). Best-effort:
// warnings only, never a failed exit — the restart itself already succeeded.
// A mismatch usually means another install shadows the updated one on PATH.
func (a *App) verifyRestartedDaemonVersion(wantVersion string) {
	want := strings.TrimPrefix(strings.TrimSpace(wantVersion), "v")
	if want == "" {
		return
	}
	deadline := time.Now().Add(15 * time.Second)
	got := ""
	for time.Now().Before(deadline) {
		ctx, cancel := contextWithTimeout(a.ctx, 2*time.Second)
		data, status, err := gatewayrt.RequestStatus(ctx, a.gatewayURL())
		cancel()
		if err == nil && status < 400 {
			var resp api.GatewayStatusResponse
			if json.Unmarshal(data, &resp) == nil && strings.TrimSpace(resp.Runtime.Version) != "" {
				got = strings.TrimPrefix(strings.TrimSpace(resp.Runtime.Version), "v")
				if got == want {
					fmt.Fprintf(a.stdout, "Daemon restarted on SelfMind %s.\n", want)
					return
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if got == "" {
		fmt.Fprintln(a.stderr, "Could not verify the restarted daemon's version (status endpoint unreachable or silent). Run `selfmind gateway status` to confirm it runs the new version.")
		return
	}
	fmt.Fprintf(a.stderr, "Warning: the restarted daemon reports version %s, expected %s. Another install may shadow the updated one — check `which -a selfmind` and your PATH order, then run `selfmind gateway restart` from the intended install.\n", got, want)
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
		fmt.Fprintln(a.stderr, updatecheck.AvailableNotice(cached.Latest))
		a.announcedUpdateVersion = cached.Latest
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
	// The result feeds the in-session TUI announcement (buffered 1, one-shot):
	// a completed check no longer waits for the NEXT startup to become
	// visible. Best-effort end to end — a failed or not-newer check sends
	// nothing and the session runs exactly as before.
	a.updateNotices = make(chan tui.UpdateNotice, 1)
	notices := a.updateNotices
	channel := effectiveChannel
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), updatecheck.RequestTimeout)
		defer cancel()
		result, err := updatecheck.Check(ctx, buildinfo.Version, channel)
		if err != nil || !shouldAnnounceUpdate(result, buildinfo.Version, channel) {
			return
		}
		select {
		case notices <- tui.UpdateNotice{Version: result.Latest}:
		default:
		}
	}()
}

// printExitUpdateHint reprints the update reminder at the session boundary —
// the one moment a `selfmind update` daemon restart cannot interrupt work
// (codex executes its actual update at the same point). Cache-only: never a
// network call on the exit path; the in-session check already refreshed the
// cache, so this reflects the latest known state.
func (a *App) printExitUpdateHint(cfg *config.Config) {
	if line := updateHintFromCacheFile(cfg, buildinfo.Version); line != "" {
		fmt.Fprintln(a.stdout, line)
	}
}

func updateHintFromCacheFile(cfg *config.Config, runningVersion string) string {
	if cfg == nil || !cfg.Updates.Enabled || isDevelopmentVersion(runningVersion) {
		return ""
	}
	cached, err := updatecheck.ReadCache(updatecheck.CachePath())
	if err != nil {
		return ""
	}
	return updateHintFromCache(cfg, runningVersion, cached)
}

// updateHintFromCache renders the exit-time reminder line, or "" when there is
// nothing to say. Pure over its inputs so the gating (channel match, newer
// version, dev build) is unit-testable without touching the real cache file.
func updateHintFromCache(cfg *config.Config, runningVersion string, cached updatecheck.Result) string {
	if cfg == nil || !cfg.Updates.Enabled || isDevelopmentVersion(runningVersion) {
		return ""
	}
	effectiveChannel := updatecheck.ResolveChannel(cfg.Updates.Channel, runningVersion)
	if !shouldAnnounceUpdate(cached, runningVersion, effectiveChannel) {
		return ""
	}
	return updatecheck.AvailableNotice(cached.Latest)
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

type updateDisposition uint8

const (
	updateInstall updateDisposition = iota
	updateRefresh
	updateSkipNewer
)

func chooseUpdateDisposition(current, available string, force bool) updateDisposition {
	if force || isDevelopmentVersion(current) {
		return updateInstall
	}
	switch updatecheck.Compare(available, current) {
	case -1:
		return updateSkipNewer
	case 0:
		return updateRefresh
	default:
		return updateInstall
	}
}

func isDevelopmentVersion(version string) bool {
	version = strings.ToLower(strings.TrimSpace(version))
	return version == "" || strings.Contains(version, "dev")
}

func runningFromNPMInstall() bool {
	return strings.TrimSpace(os.Getenv("SELFMIND_NPM_LAUNCHER")) != "" ||
		strings.EqualFold(strings.TrimSpace(os.Getenv("SELFMIND_NPM_PACKAGE")), "@selfmind/cli")
}
