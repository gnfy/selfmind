package cliapp

import (
	"fmt"
	"net"
	"net/url"
	"strings"

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

// gatewayTargetIsLocal reports whether the resolved gateway endpoint is a
// loopback address we may auto-start. A non-loopback host (a remote daemon) is
// left alone.
func (a *App) gatewayTargetIsLocal() bool {
	host := hostFromGatewayURL(a.gatewayURL())
	if host == "" {
		// Empty host (":8765") binds all interfaces but still serves loopback;
		// auto-start is reasonable.
		return true
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func hostFromGatewayURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		if h := u.Hostname(); h != "" {
			return h
		}
	}
	// Fall back to host:port parsing for bare addresses.
	if h, _, err := net.SplitHostPort(raw); err == nil {
		return h
	}
	return raw
}
