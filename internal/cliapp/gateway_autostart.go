package cliapp

import (
	"fmt"

	gatewayrt "selfmind/internal/runtime/gateway"
)

// ensureLocalGateway converges the CLI client on a running local daemon before
// it issues its first request. This is what makes the daemon-client model
// hands-free: a user can run `selfmind send ...` (or the gateway REPL) without
// having started `selfmind gateway run` first — the daemon is auto-started and
// shared by every terminal, so concurrency control (worker pool, single auth
// manager, per-workspace serialization, one control.db owner) all live in that
// one process instead of being raced across independent CLI processes.
//
// It is a no-op when the target gateway is remote (an explicit non-loopback
// URL/addr): we never spawn a daemon on the user's behalf for a remote host.
// Auto-start runs at most once per invocation; EnsureRunning is itself
// idempotent, so a second call would just health-check an already-up daemon.
func (a *App) ensureLocalGateway() {
	if a.gatewayEnsured {
		return
	}
	a.gatewayEnsured = true
	if !a.gatewayTargetIsLocal() {
		return
	}
	res, err := gatewayrt.EnsureRunning(a.ctx, gatewayrt.EnsureOptions{ConfigPath: a.configPath})
	if err != nil {
		// Non-fatal: fall through and let the actual request surface a clear
		// connection error. Auto-start is a convenience, not a hard dependency.
		fmt.Fprintf(a.stderr, "warning: could not start local gateway: %v\n", err)
		return
	}
	if res.Started {
		fmt.Fprintln(a.stderr, "Started local SelfMind gateway.")
	}
}

// gatewayTargetIsLocal reports whether the resolved gateway endpoint is a daemon
// on this host: loopback, localhost, or a wildcard address the daemon binds on
// every interface. Such a daemon is reached on its local control socket, and may
// be auto-started. A remote daemon is left alone. The runtime answers the same
// question with the same predicate, so the two never disagree.
func (a *App) gatewayTargetIsLocal() bool {
	return gatewayrt.LocalGatewayURL(a.gatewayURL())
}
