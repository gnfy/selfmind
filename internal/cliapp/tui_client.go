package cliapp

import (
	"fmt"
	"os"
	"strings"

	appcore "selfmind/internal/app"
	tui "selfmind/internal/gateway/cli"
	gwclient "selfmind/internal/gateway/client"
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
// started, so the caller can fall back to the in-process path — a
// misconfigured/first-run environment still gets a working TUI.
func (a *App) tryRunTUIClient(cfg *config.Config) (int, bool) {
	res, err := gatewayrt.EnsureRunning(a.ctx, gatewayrt.EnsureOptions{ConfigPath: a.configPath})
	if err != nil {
		fmt.Fprintf(a.stderr, "could not reach or start the gateway daemon: %v\n", err)
		return 0, false
	}
	if res.Started {
		fmt.Fprintln(a.stderr, "Started local SelfMind gateway daemon.")
	}

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
	ctrl.SetMessageProcessor(client.ProcessMessage)
	ctrl.SetClientMode(true)
	// Agent-backed slash commands (/skills, /memory subcommands, /bundles,
	// /checkpoint) run on the daemon via the safelisted /v1/dispatch endpoint.
	ctrl.SetToolDispatch(client.Dispatch)
	// Inline approval prompts: when the daemon blocks a run for tool approval,
	// the event bridge surfaces it and this responder answers it.
	ctrl.SetApprovalResponder(client.RespondApproval)

	ctrl.Start()
	fmt.Fprintln(a.stdout, "Goodbye!")
	return 0, true
}
