package cliapp

import (
	"context"
	"fmt"
	"os"
	"strings"

	appcore "selfmind/internal/app"
	"selfmind/internal/gateway/api"
	tui "selfmind/internal/gateway/cli"
	gwclient "selfmind/internal/gateway/client"
	"selfmind/internal/gateway/httpapi"
	"selfmind/internal/platform/config"
	gatewayrt "selfmind/internal/runtime/gateway"
)

// tryRunTUIClient runs the rich TUI as a thin client to a local gateway daemon.
// It auto-starts the daemon if needed, then drives the same Bubble Tea
// controller used by the in-process path — but with the chat run routed over
// the gateway HTTP API (final answer) plus a best-effort event bridge for live
// tool/thinking progress, and agent-backed slash commands routed via the
// daemon's safelisted /v1/dispatch. There is no in-process agent, gateway,
// control store, or memory store in this process, so multiple terminals share
// the one daemon's state instead of racing it.
//
// It returns (exitCode, true) once the client TUI has run. It returns
// (0, false) WITHOUT running anything if the daemon could not be reached or
// started; the caller then fails with actionable guidance — there is NO
// in-process fallback (daemon-only, ACTIVE PLAN P0-3: a silent local agent
// would write person-state into a divergent partition).
func (a *App) tryRunTUIClient(cfg *config.Config) (int, bool) {
	res, err := gatewayrt.EnsureRunning(a.ctx, gatewayrt.EnsureOptions{ConfigPath: a.configPath})
	if err != nil {
		fmt.Fprintf(a.stderr, "could not reach or start the gateway daemon: %v\n", err)
		fmt.Fprintln(a.stderr, "Common causes: a stale gateway.lock from a killed daemon, the configured gateway.addr port being taken by another process, or a broken config.yaml.")
		return 0, false
	}
	if res.Started {
		fmt.Fprintln(a.stderr, "Started local SelfMind gateway daemon.")
	}
	warnGatewayBuildMismatch(a.ctx, res.URL, a.stderr)

	tenantID := os.Getenv("SELF_TENANT_ID")
	if tenantID == "" {
		tenantID = "default"
	}

	token := strings.TrimSpace(os.Getenv("SELF_GATEWAY_TOKEN"))
	if token == "" {
		token = strings.TrimSpace(os.Getenv("SELF_DAEMON_TOKEN"))
	}
	if token == "" {
		token = strings.TrimSpace(cfg.Gateway.Token)
	}

	client := gwclient.New(res.URL, token)
	displayProvider, displayModel, _ := appcore.ResolveModelDisplay(cfg)
	// nil agent/gateway: the run path uses the message processor (the daemon),
	// and client mode gates agent-backed slash commands so nothing dereferences
	// the absent in-process agent.
	ctrl := tui.NewControllerWithGateway(nil, nil, nil, displayProvider, displayModel, cfg, tenantID)
	ctrl.SetSessionChannel(a.resumeChannel)
	// One person-scoped SSE stream stays open for the TUI lifetime. The POST
	// itself is deliberately non-streaming: queued requests return immediately,
	// while their later daemon runs continue to render through this watcher.
	ctrl.SetMessageProcessor(client.ProcessMessageDetached)
	ctrl.SetEventWatcher(func(ctx context.Context, observer httpapi.StreamObserver, onEvent func(api.RunEvent)) {
		client.WatchEvents(ctx, tenantID, observer, onEvent)
	})
	if err := a.pinResumeTask(client.ProcessMessage); err != nil {
		fmt.Fprintf(a.stderr, "SelfMind resume error: %v\n", err)
		return 1, true
	}
	ctrl.SetClientMode(true)
	// Agent-backed slash commands (/skills, /memory subcommands, /bundles,
	// /checkpoint) run on the daemon via the safelisted /v1/dispatch endpoint.
	ctrl.SetToolDispatch(client.Dispatch)
	// Inline approval prompts: when the daemon blocks a run for tool approval,
	// the event bridge surfaces it and this responder answers it.
	ctrl.SetApprovalResponder(client.RespondApproval)
	// Mid-turn steering: input typed while the daemon executes a turn is
	// forwarded into that run over the gateway API — the controller's local
	// steering channel cannot cross the process boundary.
	ctrl.SetSteerFunc(client.SteerRun)
	// Re-attach to a mid-flight daemon run (G0-d): when the attach digest
	// reports one, the controller watches its live events without a user turn.
	ctrl.SetRunWatcher(client.WatchRun)
	// Attach digest (G0-c). Ordering is load-bearing: the digest must be
	// fetched BEFORE the first presence beat below, because that beat stamps
	// accounts.last_seen_at — the very "since last CLI presence" anchor the
	// digest is computed from. Best-effort: a failed fetch just skips the
	// digest, it never blocks the TUI.
	if digest, err := client.Digest(a.ctx); err == nil {
		ctrl.SetStartupDigest(digest)
		// Learn the person's effective approval mode so the status bar shows it
		// from startup (the daemon owns it via person_settings /mode).
		ctrl.SetPersistedApprovalMode(digest.ApprovalMode)
	}
	// In-session update announcement: the background check fired at startup
	// (printUpdateNotice) delivers its result into the live session instead of
	// waiting for the next launch. Deduped against the startup notice.
	ctrl.SetUpdateNotices(a.updateNotices, a.announcedUpdateVersion)
	// Idle-TUI presence heartbeat: the event poller only runs mid-turn, so
	// without this loop an open-but-idle TUI reads as detached and CLI-origin
	// approval prompts would ALSO push to IM (double notification). Stopped on
	// TUI exit so presence expires and routing falls back to the preferred IM.
	// A live process keeps claiming attachment. An unanswered approval still
	// escalates to IM after pending_notify_after, so keyboard silence never
	// masquerades as a closed terminal.
	stopPresence := client.StartPresencePing(a.ctx)
	defer stopPresence()

	ctrl.Start()
	if a.resumeChannel != "" || ctrl.HasConversationHistory() {
		printResumeHint(a.stdout, ctrl.SessionChannel())
	}
	// Session boundary = the safe moment for `selfmind update` (daemon restart
	// interrupts nothing). Cache-only, never a network call.
	a.printExitUpdateHint(cfg)
	fmt.Fprintln(a.stdout, "Goodbye!")
	return 0, true
}
