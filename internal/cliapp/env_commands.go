package cliapp

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
	gatewayrt "selfmind/internal/runtime/gateway"
	"selfmind/internal/tools"
)

// `selfmind env` — the local environment surface.
//
// A long-lived daemon binds each run to ONE environment snapshot, which is what
// stops a command's PATH or account from changing mid-run. The cost is that a
// change the operator makes afterwards (a new toolchain, a re-login) is not
// picked up until a new snapshot exists. Refresh is therefore an explicit,
// local-only act rather than a periodic re-read: a timed re-read would change
// execution semantics at a moment nobody chose.
//
// Only the local CLI may refresh. A remote CLI or an IM message must never be
// able to change what the execution side runs.
func (a *App) runEnvCommandIfRequested() (bool, int) {
	if len(a.args) < 2 || a.args[1] != "env" {
		return false, 0
	}
	args := a.args[2:]
	if len(args) == 0 {
		fmt.Fprintln(a.stderr, "usage: selfmind env [show|refresh]")
		return true, 2
	}
	switch args[0] {
	case "show":
		return true, a.runEnvShow()
	case "refresh":
		return true, a.runEnvRefresh(args[1:])
	default:
		fmt.Fprintln(a.stderr, "usage: selfmind env [show|refresh]")
		return true, 2
	}
}

func (a *App) runEnvShow() int {
	snapshot := tools.SampleEnvironmentSnapshot(os.Environ(), "cli")
	fmt.Fprintln(a.stdout, "Execution environment (as this CLI sees it)")
	fmt.Fprintf(a.stdout, "Environment fingerprint: %s\n", shortFingerprint(snapshot.EnvironmentFingerprint))
	fmt.Fprintf(a.stdout, "Credential source hash: %s\n", shortFingerprint(snapshot.CredentialSourceHash))
	fmt.Fprintf(a.stdout, "Principal fingerprint: %s\n", shortFingerprint(snapshot.PrincipalFingerprint))
	if snapshot.VolatileCount > 0 {
		fmt.Fprintf(a.stdout, "Session-scoped PATH entries ignored: %d\n", snapshot.VolatileCount)
	}
	fmt.Fprintln(a.stdout, "Variable names and values are never printed.")
	fmt.Fprintln(a.stdout, "Run `selfmind env refresh` after changing your toolchain or logging in again.")
	return 0
}

// runEnvRefresh re-samples the operator environment from a LOGIN shell and, with
// --restart, starts the daemon with that sample.
//
// The comparison baseline is the RUNNING DAEMON, not this CLI. Comparing against
// the CLI reported "unchanged" in exactly the situation the command exists for:
// the CLI is normally the first process to see a new toolchain, so a fresh shell
// and a stale daemon look identical from here. When the daemon's identity cannot
// be read, the sample is treated as new rather than assumed current — refusing to
// act on missing information would leave the operator with no way forward.
//
// The sample is handed DIRECTLY to the restarted daemon and never written to
// disk: an environment file would persist credential values that must only exist
// in memory.
func (a *App) runEnvRefresh(args []string) int {
	fs := flag.NewFlagSet("selfmind env refresh", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	restart := fs.Bool("restart", false, "restart the gateway with the sampled environment so new runs adopt it")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	sampled, err := sampleLoginShellEnvironment()
	if err != nil {
		fmt.Fprintf(a.stderr, "Could not sample a login shell environment: %v\n", err)
		return 1
	}
	after := tools.SampleEnvironmentSnapshot(sampled, "login-shell")
	daemon, daemonKnown := a.daemonEnvironmentIdentity()

	changed := daemonEnvironmentDifferences(daemon, after)
	switch {
	case !daemonKnown:
		fmt.Fprintln(a.stdout, "The gateway is not running or did not report its environment; treating the sample as new.")
	case len(changed) == 0:
		fmt.Fprintln(a.stdout, "The gateway already runs this environment; nothing to refresh.")
		if !*restart {
			return 0
		}
		// --restart is an explicit instruction. Honour it: the operator may be
		// recovering from a daemon that is wedged for an unrelated reason, and
		// silently doing nothing after being told to restart is worse than an
		// unnecessary restart.
		fmt.Fprintln(a.stdout, "Restarting anyway because --restart was given.")
	default:
		fmt.Fprintln(a.stdout, "The gateway is running an older environment:")
		for _, dimension := range changed {
			fmt.Fprintf(a.stdout, "  %s changed\n", dimension)
		}
	}

	if !*restart {
		// Do NOT claim a plain restart adopts this: it inherits the CLI's own
		// environment, which is not necessarily the sample.
		fmt.Fprintln(a.stdout, "Run `selfmind env refresh --restart` to adopt it for new runs.")
		fmt.Fprintln(a.stdout, "A plain `selfmind gateway restart` inherits this shell's environment instead.")
		if launchdManagesGateway() {
			fmt.Fprintln(a.stdout, "This daemon is managed by launchd, which pins its own environment: "+
				"re-run `selfmind gateway service install` from a shell that has the new values.")
		}
		return 0
	}
	// A launchd-managed daemon takes its environment from the plist, so a restart
	// cannot adopt a sample. Refusing is the only honest outcome: reporting
	// success here would tell the operator the new environment is live when the
	// service definition still pins the old one.
	if launchdManagesGateway() {
		fmt.Fprintln(a.stderr, "This daemon is managed by launchd, which pins its environment in the service definition.")
		fmt.Fprintln(a.stderr, "A restart cannot adopt a sampled environment. Run `selfmind gateway service install` "+
			"from a shell that has the new values, which rewrites the service definition, then `selfmind gateway restart`.")
		return 1
	}
	fmt.Fprintln(a.stdout, "Restarting the gateway with the sampled environment...")
	fmt.Fprintln(a.stdout, "Runs already in flight keep the environment they started with.")
	return a.gatewayRestartWithEnvironment(nil, sampled)
}

// daemonEnvironmentIdentity reads the running daemon's non-secret environment
// fingerprints. ok is false when the daemon is unreachable or predates the field.
func (a *App) daemonEnvironmentIdentity() (api.GatewayRuntimeInfo, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, code, err := gatewayrt.RequestStatus(ctx, a.gatewayURL())
	if err != nil || code != 200 {
		return api.GatewayRuntimeInfo{}, false
	}
	var status api.GatewayStatusResponse
	if json.Unmarshal(data, &status) != nil {
		return api.GatewayRuntimeInfo{}, false
	}
	if strings.TrimSpace(status.Runtime.EnvironmentFingerprint) == "" &&
		strings.TrimSpace(status.Runtime.PrincipalFingerprint) == "" {
		return api.GatewayRuntimeInfo{}, false
	}
	return status.Runtime, true
}

// daemonEnvironmentDifferences names the non-secret dimensions where a fresh
// sample differs from what the daemon is running.
func daemonEnvironmentDifferences(daemon api.GatewayRuntimeInfo, sample *executionenv.Snapshot) []string {
	if sample == nil {
		return nil
	}
	changed := make([]string, 0, 3)
	if daemon.PrincipalFingerprint != sample.PrincipalFingerprint {
		changed = append(changed, "account/profile")
	}
	if daemon.EnvironmentFingerprint != sample.EnvironmentFingerprint {
		changed = append(changed, "PATH/HOME/proxy")
	}
	if daemon.CredentialSourceHash != sample.CredentialSourceHash {
		changed = append(changed, "credential source")
	}
	return changed
}

// sampleLoginShellEnvironment reads the environment a login shell produces. The
// probe is bounded and read-only: it prints the environment and exits.
func sampleLoginShellEnvironment() ([]string, error) {
	shell := strings.TrimSpace(os.Getenv("SHELL"))
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.Command(shell, "-lc", "env -0")
	cmd.Env = tools.BuildProcessEnv(os.Environ(), tools.DefaultProcessEnvPolicy())
	done := make(chan struct{})
	var out []byte
	var runErr error
	go func() {
		out, runErr = cmd.Output()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return nil, fmt.Errorf("login shell probe timed out")
	}
	if runErr != nil {
		return nil, runErr
	}
	entries := strings.Split(string(out), "\x00")
	sampled := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.Contains(entry, "=") {
			sampled = append(sampled, entry)
		}
	}
	if len(sampled) == 0 {
		return nil, fmt.Errorf("login shell produced no environment")
	}
	return sampled, nil
}

func shortFingerprint(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "(none)"
	}
	if len(value) > 12 {
		return value[:12]
	}
	return value
}

// envRuntimeRootForDiagnostics exposes the configured runtime root without
// importing the gateway, for `selfmind doctor`.
func envRuntimeRootForDiagnostics() string { return executionenv.RuntimeRoot() }
