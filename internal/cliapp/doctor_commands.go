package cliapp

// `selfmind doctor` — self-serve diagnostics (observability export). One
// command produces a redacted bundle the owner can hand over instead of
// describing symptoms by hand: gateway status, recent runs, pending approvals,
// queued tasks, unconfirmed/failed outbound pushes, a presence snapshot, the
// gateway log tail, and per-channel activity. Read-only, and it works whether
// or not the daemon is up: live gateway status when reachable, else the durable
// state in control.db and the on-disk log.

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	appcore "selfmind/internal/app"
	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/platform/config"
	gatewayrt "selfmind/internal/runtime/gateway"
	"selfmind/internal/tools"
)

const (
	doctorRecentRuns   = 10
	doctorRecentErrors = 12
	doctorRecentEvents = 40
	doctorMaxPushes    = 10
	doctorLogLines     = 50
	// presenceRecentWindow labels an account as recently active in the presence
	// snapshot. Matches the daemon's presence TTL intent (durable last_seen_at is
	// the only cross-process signal a CLI-side doctor can read).
	presenceRecentWindow = 90 * time.Second
)

func (a *App) runDoctorCommandIfRequested() (bool, int) {
	if len(a.args) < 2 || a.args[1] != "doctor" {
		return false, 0
	}
	return true, a.doctor(a.args[2:])
}

func (a *App) doctor(args []string) int {
	fs := flag.NewFlagSet("selfmind doctor", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	outPath := fs.String("out", "", "write the bundle to a file instead of stdout")
	probeModels := fs.Bool("probe-models", false, "send one bounded live request per unique configured role model")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	dataDir := a.gatewayDataDir()
	store, err := control.OpenStore(dataDir)
	if err != nil {
		fmt.Fprintf(a.stderr, "doctor: cannot open control store: %v\n", err)
		return 1
	}
	defer store.Close()

	ctx, cancel := contextWithTimeout(a.ctx, 10*time.Second)
	defer cancel()

	// Diagnostics are read-only. Resolve the same platform identity used by
	// normal CLI requests, but never create a phantom person merely by running
	// doctor on a fresh or differently configured installation.
	identity, err := resolveDoctorIdentity(ctx, store, a.tenantID(), platformUserID())
	if err != nil {
		fmt.Fprintf(a.stderr, "doctor: cannot resolve identity: %v\n", err)
		return 1
	}

	configSection := a.collectConfigDiagnostics().section()
	report := buildDoctorReport(ctx, store, identity, dataDir, a.gatewayStatusLine(), configSection, doctorLogLines)
	probeFailed := false
	if *probeModels {
		cfg, loadErr := config.LoadConfig(config.Options{Path: a.configPath})
		if loadErr != nil {
			report += "\n\n== Model role probes ==\n(error: " + loadErr.Error() + ")"
			probeFailed = true
		} else {
			probeCtx, probeCancel := context.WithTimeout(a.ctx, 90*time.Second)
			probes := appcore.ProbeConfiguredModelRoles(probeCtx, cfg)
			probeCancel()
			section, failed := formatModelRoleProbes(probes)
			report += "\n\n" + section
			probeFailed = failed
		}
	}

	if strings.TrimSpace(*outPath) != "" {
		if err := os.WriteFile(*outPath, []byte(report), 0600); err != nil {
			fmt.Fprintf(a.stderr, "doctor: cannot write %s: %v\n", *outPath, err)
			return 1
		}
		fmt.Fprintf(a.stdout, "Diagnostic bundle written to %s\n", *outPath)
		if probeFailed {
			return 1
		}
		return 0
	}
	fmt.Fprintln(a.stdout, report)
	if probeFailed {
		return 1
	}
	return 0
}

func resolveDoctorIdentity(ctx context.Context, store *control.Store, tenantID, userID string) (*control.IdentityContext, error) {
	identity, err := store.ResolveAccount(ctx, tenantID, "cli", userID)
	if err != nil {
		return nil, err
	}
	if identity != nil {
		return identity, nil
	}
	return &control.IdentityContext{
		TenantID:       tenantID,
		Platform:       "cli",
		PlatformUserID: userID,
	}, nil
}

func formatModelRoleProbes(probes []appcore.ModelRoleProbe) (string, bool) {
	var sb strings.Builder
	sb.WriteString("== Model role probes ==\n")
	if len(probes) == 0 {
		sb.WriteString("(no explicitly configured roles)")
		return sb.String(), false
	}
	failed := false
	for _, probe := range probes {
		roles := strings.Join(probe.Roles, ",")
		if probe.Err != nil {
			failed = true
			fmt.Fprintf(&sb, "- FAIL roles=%s provider=%s model=%s latency=%s error=%s\n",
				roles, valueOrUnknown(probe.Provider), valueOrUnknown(probe.Model), probe.Latency.Round(time.Millisecond),
				oneLine(tools.RedactSensitive(probe.Err.Error()), 180))
			continue
		}
		fmt.Fprintf(&sb, "- OK roles=%s provider=%s model=%s latency=%s\n",
			roles, valueOrUnknown(probe.Provider), valueOrUnknown(probe.Model), probe.Latency.Round(time.Millisecond))
	}
	return strings.TrimSpace(sb.String()), failed
}

func valueOrUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}
	return strings.TrimSpace(value)
}

// tenantID resolves the CLI's default tenant, mirroring the gateway runner.
func (a *App) tenantID() string {
	if t := strings.TrimSpace(os.Getenv("SELF_TENANT_ID")); t != "" {
		return t
	}
	return control.DefaultTenantID
}

// gatewayStatusLine returns a one-line gateway status, preferring the live HTTP
// status endpoint and falling back to the on-disk PID record so doctor still
// reports something useful when the daemon is unreachable or down.
func (a *App) gatewayStatusLine() string {
	serviceState := strings.TrimSpace(gatewayServiceDoctorLine())
	withService := func(status string) string {
		if serviceState == "" {
			return status
		}
		return status + " " + serviceState
	}
	ctx, cancel := contextWithTimeout(a.ctx, 3*time.Second)
	defer cancel()
	if data, code, err := gatewayrt.RequestStatus(ctx, a.gatewayURL()); err == nil && code < 400 {
		var status api.GatewayStatusResponse
		if json.Unmarshal(data, &status) == nil {
			rt := status.Runtime
			buildState := gatewayBuildState(rt.BuildFingerprint)
			return withService(fmt.Sprintf("running (state=%s pid=%d addr=%s active_runs=%d build=%s)", status.State, rt.PID, rt.Addr, status.ActiveRunCount, buildState))
		}
	}
	manager := gatewayrt.NewManager(a.gatewayDataDir(), "")
	if rec, ok := manager.RunningRecord(); ok {
		if rec.HeartbeatStale(time.Now()) {
			return withService(fmt.Sprintf("unreachable (pid=%d heartbeat stale since=%s); inspect daemon logs", rec.PID, rec.HeartbeatAt))
		}
		return withService(fmt.Sprintf("running (pid=%d addr=%s), HTTP status unavailable", rec.PID, rec.Addr))
	}
	if rec, err := gatewayrt.ReadStatusRecord(manager.Paths.StatePath); err == nil && rec.State == "crashed" {
		return withService(fmt.Sprintf("crashed (instance=%s reason=%s last_heartbeat=%s)", rec.InstanceID, rec.ExitReason, rec.HeartbeatAt))
	}
	return withService("not running")
}

// buildDoctorReport assembles the redacted diagnostic bundle from durable
// control-plane state and the on-disk log. It is separated from CLI plumbing so
// it can be unit-tested against a seeded temp store.
func buildDoctorReport(ctx context.Context, store *control.Store, identity *control.IdentityContext, dataDir, gatewayStatus, configSection string, logLines int) string {
	var sb strings.Builder
	sb.WriteString("SelfMind doctor — diagnostic bundle\n")
	fmt.Fprintf(&sb, "generated: %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&sb, "person: %s  tenant: %s\n", identity.PersonID, identity.TenantID)
	fmt.Fprintf(&sb, "data dir: %s\n\n", dataDir)

	fmt.Fprintf(&sb, "== Gateway ==\n%s\n\n", gatewayStatus)
	if strings.TrimSpace(configSection) != "" {
		sb.WriteString(configSection)
		sb.WriteString("\n\n")
	}

	sb.WriteString("== Workspace trust ==\n")
	if workspaces, err := store.ListWorkspaces(ctx, identity.TenantID, identity.PersonID); err != nil {
		fmt.Fprintf(&sb, "(error: %v)\n", err)
	} else {
		pending := 0
		for _, workspace := range workspaces {
			if workspace.TrustSource != "migration_review_required" {
				continue
			}
			pending++
			fmt.Fprintf(
				&sb,
				"- review required: %s (%s)\n    %s\n",
				oneLine(workspace.Name, 60),
				workspace.ID,
				oneLine(workspace.LocalPath, 160),
			)
		}
		if pending == 0 {
			sb.WriteString("no migrated workspaces require trust review\n")
		} else {
			sb.WriteString("Review each path, then run `selfmind ws trust <workspace_id>` or leave it untrusted.\n")
		}
	}
	sb.WriteString("\n")

	// Recent runs.
	sb.WriteString("== Recent runs ==\n")
	if runs, err := store.ListRecentRunsForPerson(ctx, identity.TenantID, identity.PersonID, doctorRecentRuns); err != nil {
		fmt.Fprintf(&sb, "(error: %v)\n", err)
	} else if len(runs) == 0 {
		sb.WriteString("(none)\n")
	} else {
		for _, r := range runs {
			title := strings.TrimSpace(r.TaskTitle)
			if title == "" {
				title = "(untitled)"
			}
			fmt.Fprintf(&sb, "- [%s] %s (%s)\n", r.Status, oneLine(title, 60), r.Elapsed().Round(time.Second))
			if strings.TrimSpace(r.LastError) != "" {
				fmt.Fprintf(&sb, "    error: %s\n", oneLine(tools.RedactSensitive(r.LastError), 160))
			}
		}
	}
	sb.WriteString("\n")

	// Recent errors: run-level failures + tool failures, aggregated newest
	// first, so "what has been going wrong lately" is one glance instead of
	// reading each run's events by hand.
	sb.WriteString("== Recent errors ==\n")
	if errs, err := store.ListRecentErrors(ctx, identity.TenantID, identity.PersonID, doctorRecentErrors); err != nil {
		fmt.Fprintf(&sb, "(error: %v)\n", err)
	} else if len(errs) == 0 {
		sb.WriteString("(none)\n")
	} else {
		for _, e := range errs {
			fmt.Fprintf(&sb, "- %s [%s:%s] %s\n",
				e.When.Format("01-02 15:04"), e.Kind, oneLine(e.Source, 24),
				oneLine(tools.RedactSensitive(e.Message), 160))
		}
	}
	sb.WriteString("\n")

	// Pending approvals.
	sb.WriteString("== Pending approvals ==\n")
	if approvals, err := store.ListApprovalRequests(ctx, identity.TenantID, identity.PersonID, "pending", 20); err != nil {
		fmt.Fprintf(&sb, "(error: %v)\n", err)
	} else if len(approvals) == 0 {
		sb.WriteString("(none)\n")
	} else {
		for _, ap := range approvals {
			fmt.Fprintf(&sb, "- %s [%s] (%s)\n", ap.ActionType, oneLine(tools.RedactSensitive(string(ap.Payload)), 120), ap.ID)
		}
	}
	sb.WriteString("\n")

	// Queued tasks.
	sb.WriteString("== Queued tasks ==\n")
	if queued, err := store.ListQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued); err != nil {
		fmt.Fprintf(&sb, "(error: %v)\n", err)
	} else if len(queued) == 0 {
		sb.WriteString("(none)\n")
	} else {
		for i, q := range queued {
			fmt.Fprintf(&sb, "%d. %s\n", i+1, oneLine(tools.RedactSensitive(q.Content), 80))
		}
	}
	sb.WriteString("\n")

	// Recent event timeline — the real per-turn/tool/approval/error detail
	// (task_events), which the sparse gateway.log does not carry.
	sb.WriteString("== Recent events ==\n")
	if events, err := store.ListRecentEventsForPerson(ctx, identity.TenantID, identity.PersonID, doctorRecentEvents); err != nil {
		fmt.Fprintf(&sb, "(error: %v)\n", err)
	} else if len(events) == 0 {
		sb.WriteString("(none)\n")
	} else {
		for _, e := range events {
			ts := e.CreatedAt.Format("01-02 15:04:05")
			line := fmt.Sprintf("%s  %-18s %s", ts, e.Type, oneLine(tools.RedactSensitive(e.Preview), 100))
			sb.WriteString(strings.TrimRight(line, " ") + "\n")
		}
	}
	sb.WriteString("\n")

	// Outbound pushes that never confirmed.
	sb.WriteString("== Unconfirmed / failed pushes ==\n")
	if pushes, err := store.ListUndeliveredOutbound(ctx, identity.TenantID, identity.PersonID, time.Unix(0, 0), doctorMaxPushes); err != nil {
		fmt.Fprintf(&sb, "(error: %v)\n", err)
	} else if len(pushes) == 0 {
		sb.WriteString("(none)\n")
	} else {
		unconfirmed := false
		for _, p := range pushes {
			if p.Status == "sent_unconfirmed" {
				unconfirmed = true
			}
			fmt.Fprintf(&sb, "- [%s/%s] %s\n", p.Platform, p.Status, oneLine(tools.RedactSensitive(p.Content), 80))
		}
		if unconfirmed {
			sb.WriteString("note: sent_unconfirmed = the platform accepted the push but delivery is doubtful.\n")
			sb.WriteString("  On WeChat/iLink this happens when the proactive-push session token (context_token)\n")
			sb.WriteString("  went stale — the API returns success yet the message never reaches the phone.\n")
			sb.WriteString("  Recovery: send ANY message from that chat; the fresh inbound renews the token and\n")
			sb.WriteString("  arms a one-shot catch-up re-push of recent unconfirmed notices (bounded, no duplicates).\n")
		}
	}
	sb.WriteString("\n")

	// Cron governance: system jobs must stay singular. The 2026-07 incident
	// registered one skill-pruner per data-directory entry (~2.5k rows) whose
	// daily fire kept touching stale eval/person partitions; this section
	// makes that class of runaway visible before it burns a night of I/O.
	sb.WriteString("== Cron governance ==\n")
	sb.WriteString(cronGovernanceSection(ctx, dataDir))
	sb.WriteString("\n")

	// Presence snapshot (durable last_seen recency per bound account).
	sb.WriteString("== Presence (bound accounts) ==\n")
	if accounts, err := store.ListAccountsByPerson(ctx, identity.TenantID, identity.PersonID); err != nil {
		fmt.Fprintf(&sb, "(error: %v)\n", err)
	} else if len(accounts) == 0 {
		sb.WriteString("(none)\n")
	} else {
		for _, acct := range accounts {
			seen := "never seen"
			attached := ""
			if acct.LastSeenAt > 0 {
				age := time.Since(time.Unix(acct.LastSeenAt, 0))
				seen = age.Round(time.Second).String() + " ago"
				if age <= presenceRecentWindow {
					attached = " (attached)"
				}
			}
			fmt.Fprintf(&sb, "- %s: last seen %s%s\n", acct.Platform, seen, attached)
		}
	}
	sb.WriteString("\n")

	// Activity by channel (durable per-person trajectory).
	sb.WriteString("== Activity by channel ==\n")
	if counts, err := store.CountChannelMessagesByChannel(ctx, identity.TenantID, identity.PersonID); err != nil {
		fmt.Fprintf(&sb, "(error: %v)\n", err)
	} else if len(counts) == 0 {
		sb.WriteString("(none)\n")
	} else {
		for _, c := range counts {
			fmt.Fprintf(&sb, "- %s: %d messages\n", c.Channel, c.Count)
		}
	}
	sb.WriteString("\n")

	// Gateway log tail.
	fmt.Fprintf(&sb, "== Gateway log (last %d lines) ==\n", logLines)
	logPath := gatewayrt.ResolvePaths(dataDir).LogPath
	if tail, err := tailLines(logPath, logLines); err != nil {
		fmt.Fprintf(&sb, "(log unavailable: %v)\n", err)
	} else if len(tail) == 0 {
		sb.WriteString("(empty)\n")
	} else {
		for _, line := range tail {
			sb.WriteString(tools.RedactSensitive(line) + "\n")
		}
	}

	return strings.TrimSpace(sb.String())
}

// oneLine collapses whitespace and bounds a string for a compact bundle line.
func oneLine(value string, max int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= max {
		return value
	}
	return value[:max] + "…"
}

// tailLines returns the last n lines of a file, or an empty slice for a missing
// file (doctor must not fail because a log was never created).
func tailLines(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return lines, nil
}

// cronGovernanceSection summarizes cron_jobs health from the cron database:
// totals, system-job count, the keyed skill-pruner, historical built-in rows,
// and duplicate system keys. Read-only; a missing db reports cleanly.
func cronGovernanceSection(ctx context.Context, dataDir string) string {
	db, err := sql.Open("sqlite", "file:"+dataDir+"/cron.db?mode=ro")
	if err != nil {
		return fmt.Sprintf("(error: %v)\n", err)
	}
	defer db.Close()
	var total, system, pruner, legacyPruner, dupGroups int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM cron_jobs").Scan(&total); err != nil {
		return fmt.Sprintf("(error: %v)\n", err)
	}
	_ = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM cron_jobs WHERE COALESCE(system_key,'') != ''").Scan(&system)
	_ = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM cron_jobs WHERE system_key = 'skill-pruner:default'").Scan(&pruner)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_jobs
		WHERE COALESCE(system_key, '') = '' AND name LIKE 'skill-pruner-%'
		  AND cron_expr = '0 3 * * *' AND channel = 'cli'
		  AND prompt LIKE 'skill_prune:%'`).Scan(&legacyPruner)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (
		SELECT 1 FROM cron_jobs WHERE COALESCE(system_key, '') != ''
		GROUP BY system_key HAVING COUNT(*) > 1)`).Scan(&dupGroups)
	out := fmt.Sprintf("jobs: %d total, %d system, skill-pruner: %d keyed + %d legacy, %d duplicate system-key group(s)\n",
		total, system, pruner, legacyPruner, dupGroups)
	if pruner > 1 {
		out += "warning: more than one skill-pruner job — skills are control-tenant assets; restart the daemon to run the governance migration.\n"
	}
	if legacyPruner > 0 {
		out += "warning: legacy system-shaped skill-pruner jobs remain; restart the daemon to run the governance migration.\n"
	}
	if dupGroups > 0 {
		out += "warning: duplicate cron rows detected — restart the daemon (EnsureJob collapses them) or inspect cron.db.\n"
	}
	if total > 50 {
		out += "warning: unusually many cron jobs — check for runaway system registration.\n"
	}
	return out
}
