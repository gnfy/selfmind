package httpapi

// /diag — the compact, from-the-phone runtime snapshot (observability, self-
// serve diagnostics). It answers "what is SelfMind doing right now and is
// anything stuck?" without the full `selfmind doctor` bundle: active run +
// elapsed, queued count, pending approvals, the last run error if any, and the
// current task's recent events. English, redacted, no raw hashes beyond the
// actionable approval ids that /approve already uses.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
	"selfmind/internal/tools"
	"selfmind/internal/tools/envprofiles"
)

const diagRecentEvents = 5

func (d *Server) toolsDiagReply(_ context.Context, _ *control.IdentityContext) (string, error) {
	if d == nil || d.ToolSchemaReportFunc == nil {
		return "Tool schema diagnostics unavailable.", nil
	}
	reports := d.ToolSchemaReportFunc()
	active, repaired, quarantined := 0, 0, 0
	attention := make([]tools.ToolSchemaReport, 0)
	for _, report := range reports {
		switch report.Status {
		case tools.ToolSchemaRepaired:
			repaired++
			attention = append(attention, report)
		case tools.ToolSchemaQuarantined:
			quarantined++
			attention = append(attention, report)
		default:
			active++
		}
	}
	var sb strings.Builder
	sb.WriteString("Tool schema diagnostics\n")
	fmt.Fprintf(&sb, "Catalog: %d active, %d repaired, %d quarantined\n", active, repaired, quarantined)
	if len(attention) == 0 {
		sb.WriteString("Status: healthy; all registered tool schemas are provider-ready.")
		return sb.String(), nil
	}
	sb.WriteString("Attention:\n")
	const maxEntries = 12
	for index, report := range attention {
		if index >= maxEntries {
			fmt.Fprintf(&sb, "- ... %d more\n", len(attention)-maxEntries)
			break
		}
		issue := "schema normalized"
		if len(report.Issues) > 0 {
			issue = report.Issues[0].Code + " at " + report.Issues[0].Path
		}
		fmt.Fprintf(&sb, "- %s [%s/%s] %s (schema %s)\n",
			report.Name, report.Origin, report.Status, issue, report.Hash)
	}
	if quarantined > 0 {
		sb.WriteString("Quarantined tools are not sent to models and cannot execute.")
	}
	return strings.TrimSpace(sb.String()), nil
}

func (d *Server) executionDiagReply(ctx context.Context, identity *control.IdentityContext) (string, error) {
	if d == nil || d.Control == nil || identity == nil {
		return "Execution diagnostics unavailable.", nil
	}
	diag := tools.ExecSandboxDiagnostics()
	var sb strings.Builder
	sb.WriteString("Execution diagnostics\n")
	fmt.Fprintf(&sb, "Platform: %s\n", diag.Platform)
	fmt.Fprintf(&sb, "Sandbox backend: %s\n", diag.Backend)
	fmt.Fprintf(&sb, "Sandbox policy: enabled=%t, required=%t, available=%t\n",
		diag.Enabled, diag.Required, diag.Available)
	fmt.Fprintf(&sb, "Sandbox network: %s\n", diag.Network)
	fmt.Fprintf(&sb, "Process environment: %s\n", diag.Environment)
	// Snapshot state: without this, "the daemon is using a stale PATH" is
	// invisible and a first-command failure has no diagnosable cause.
	if snapshot := executionenv.DefaultRegistry().Current(); snapshot != nil {
		fmt.Fprintf(&sb, "Environment snapshot: generation %d, sampled %s ago, source %s\n",
			snapshot.Generation, compactDuration(snapshot.Age(time.Now())), fallback(snapshot.Source, "inherited"))
		if snapshot.VolatileCount > 0 {
			fmt.Fprintf(&sb, "Environment volatile entries dropped: %d\n", snapshot.VolatileCount)
		}
	} else {
		sb.WriteString("Environment snapshot: none installed\n")
	}
	if root := executionenv.RuntimeRoot(); root != "" {
		sb.WriteString("Run scratch: available\n")
	} else {
		sb.WriteString("Run scratch: unavailable (falling back to a private tmpfs)\n")
	}
	fmt.Fprintf(&sb, "Environment profiles available: %s\n", strings.Join(envprofiles.IDs(), ", "))
	if ws, err := d.Control.CurrentWorkspace(ctx, identity.TenantID, identity.PersonID); err == nil && ws != nil {
		fmt.Fprintf(&sb, "Workspace: %s\n", ws.Name)
		fmt.Fprintf(&sb, "Workspace trust: %s\n", fallback(ws.TrustLevel, "untrusted"))
		fmt.Fprintf(&sb, "Writable root: %s\n", ws.LocalPath)
		if len(ws.AllowedRoots) > 1 {
			fmt.Fprintf(&sb, "Allowed roots: %d\n", len(ws.AllowedRoots))
		}
		if grants, err := d.Control.ListActiveExecutionCapabilities(ctx, identity.TenantID, identity.PersonID, ws.ID); err == nil {
			names := make([]string, 0, len(grants))
			for _, grant := range grants {
				names = append(names, grant.Capability)
			}
			sort.Strings(names)
			if len(names) == 0 {
				sb.WriteString("Workspace capabilities: none\n")
			} else {
				fmt.Fprintf(&sb, "Workspace capabilities: %s\n", strings.Join(names, ", "))
			}
		}
	} else {
		sb.WriteString("Workspace: none\n")
	}
	active := d.coordinator().currentActive(identity.PersonID)
	if active != nil && active.RunID != "" {
		if lease, err := d.Control.GetExecutionLeaseByRun(ctx, identity.TenantID, active.RunID); err == nil && lease != nil {
			fmt.Fprintf(&sb, "Environment lease: %s (%s snapshot)\n", shortOpaqueID(lease.ID), lease.EnvironmentProfile)
			fmt.Fprintf(&sb, "Credential references: %d hidden\n", len(lease.CredentialRefs))
			if lease.EnvironmentGeneration > 0 {
				fmt.Fprintf(&sb, "Lease environment generation: %d\n", lease.EnvironmentGeneration)
			}
			if bytes, err := executionenv.ScratchBytes(lease.ID); err == nil {
				fmt.Fprintf(&sb, "Run scratch size: %s%s\n", compactBytes(bytes), scratchQuotaNote(bytes))
			}
		}
		if summary := d.recentExecutionSummary(ctx, identity.TenantID, active.TaskID, active.RunID); summary != "" {
			sb.WriteString(summary)
		}
		if scope := tools.ExecutionScopeDiagnostics(identity.PersonID); scope.Installed {
			profile := strings.TrimSpace(scope.ExecutionProfile)
			if profile == "" {
				profile = "default"
			}
			fmt.Fprintf(&sb, "Execution profile: %s\n", profile)
		}
	} else {
		sb.WriteString("Environment lease: none (no active run)\n")
	}
	sb.WriteString("Credential values: hidden\n")
	return strings.TrimSpace(sb.String()), nil
}

func shortOpaqueID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

func (d *Server) diagReply(ctx context.Context, identity *control.IdentityContext) (string, error) {
	if d == nil || d.Control == nil || identity == nil {
		return "Diagnostics unavailable.", nil
	}
	var sb strings.Builder
	sb.WriteString("SelfMind diagnostics\n")

	// Active run.
	active := d.coordinator().currentActive(identity.PersonID)
	var currentTaskID string
	if active != nil {
		currentTaskID = active.TaskID
		title := strings.TrimSpace(active.Summary)
		if title == "" {
			title = "(starting)"
		}
		fmt.Fprintf(&sb, "Active run: %s (%s elapsed)\n", truncate(toOneLine(title), 60), time.Since(active.StartedAt).Round(time.Second))
	} else {
		sb.WriteString("Active run: none\n")
	}

	// Queued count.
	queued, _ := d.Control.CountQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
	fmt.Fprintf(&sb, "Queued: %d\n", queued)
	if stats, err := d.Control.ReadTaskGovernanceStats(ctx, identity.TenantID, identity.PersonID); err == nil {
		fmt.Fprintf(&sb, "Tasks: open %d, terminal %d, archived %d, pinned %d, inbox runs %d\n",
			stats.Open, stats.Terminal, stats.Archived, stats.Pinned, stats.InboxRuns)
	}
	if health, err := d.Control.EvolutionHealthForPerson(ctx, identity.TenantID, identity.PersonID); err == nil {
		candidateTotal := health.Statuses["candidate"] + health.Statuses["shadow"] + health.Statuses["eligible"] + health.Statuses["enabled"] + health.Statuses["degraded"]
		if d.SelfEvolution.Enabled || health.Profiles24h > 0 || candidateTotal > 0 {
			fmt.Fprintf(&sb, "Self-evolution: profiles %d (24h), candidate %d, shadow %d, eligible %d, enabled %d, degraded %d, fallbacks %d\n",
				health.Profiles24h, health.Statuses["candidate"], health.Statuses["shadow"], health.Statuses["eligible"],
				health.Statuses["enabled"], health.Statuses["degraded"], health.Fallbacks)
		}
	}
	if watches, err := d.Control.CountExternalWatchesByStatus(ctx, identity.TenantID, identity.PersonID); err == nil {
		activeWatches := watches[control.ExternalWatchPending] + watches[control.ExternalWatchRunning]
		if activeWatches+watches[control.ExternalWatchFailed]+watches[control.ExternalWatchTimedOut]+watches[control.ExternalWatchBlocked] > 0 {
			fmt.Fprintf(&sb, "External watches: active %d, failed %d, timed out %d, blocked %d\n",
				activeWatches, watches[control.ExternalWatchFailed], watches[control.ExternalWatchTimedOut],
				watches[control.ExternalWatchBlocked])
		}
		// A blocked watch is the actionable one: it stopped because its own check
		// could not observe anything, so the operator needs the reason and the
		// remedy here rather than in the run history.
		if watches[control.ExternalWatchBlocked] > 0 {
			if blocked, err := d.Control.ListExternalWatchesFinishedSinceForPerson(ctx, identity.TenantID, identity.PersonID, control.ExternalWatchBlocked, time.Now().Add(-externalWatchRecoveryLookback), 10); err == nil {
				for _, watch := range blocked {
					reason, _ := watchCheckDefect(watch.LastError)
					fmt.Fprintf(&sb, "- watch blocked: %s | %s | class %s | %s\n",
						truncate(toOneLine(watch.Description), 48),
						firstNonEmpty(reason, "check_failed"),
						firstNonEmpty(strings.TrimSpace(watch.FailureClass), "unknown"),
						truncate(toOneLine(strings.TrimSpace(watch.LastError)), 120))
				}
			}
		}
		// A timed_out watch whose recorded output already matches a declared
		// terminal pattern is a misjudgment the startup recovery pass will
		// revise — surface it instead of leaving a silently wrong verdict.
		if watches[control.ExternalWatchTimedOut] > 0 {
			if finished, err := d.Control.ListExternalWatchesFinishedSinceForPerson(ctx, identity.TenantID, identity.PersonID, control.ExternalWatchTimedOut, time.Now().Add(-externalWatchRecoveryLookback), 10); err == nil {
				for _, watch := range finished {
					if status := classifyStoredExternalWatchOutput(watch); status != "" {
						fmt.Fprintf(&sb, "- watch verdict suspect: %s timed out but last output matches %s (%s)\n",
							truncate(toOneLine(watch.Description), 60), status,
							truncate(toOneLine(strings.TrimSpace(watch.LastOutput)), 40))
					}
				}
			}
		}
	}
	if health, err := d.Control.MaintenanceHealthForPerson(ctx, identity.TenantID, identity.PersonID); err == nil {
		if health.Pending > 0 || health.Failed > 0 || health.Running > 0 {
			oldest := ""
			if !health.OldestPendingAt.IsZero() {
				oldest = fmt.Sprintf(", oldest %s", time.Since(health.OldestPendingAt).Round(time.Second))
			}
			fmt.Fprintf(&sb, "Background learning: queued %d, retrying %d, running %d (batched%s)\n", health.Pending, health.Failed, health.Running, oldest)
		}
		if health.Blocked > 0 {
			fmt.Fprintf(&sb, "Background learning: paused (%d job(s))\n", health.Blocked)
			for i, route := range health.BlockedRoutes {
				if i >= 3 {
					break
				}
				nextProbe := "waiting for probe"
				if !route.NextProbeAt.IsZero() {
					remaining := time.Until(route.NextProbeAt).Round(time.Second)
					if remaining <= 0 {
						nextProbe = "probe due"
					} else {
						nextProbe = "next probe in " + remaining.String()
					}
				}
				fmt.Fprintf(&sb, "- route: %s/%s, %s\n", route.Provider, route.Model, nextProbe)
			}
			if reason := strings.TrimSpace(health.LastError); reason != "" {
				fmt.Fprintf(&sb, "- provider: %s\n", truncate(toOneLine(tools.RedactSensitive(reason)), 140))
			}
		} else if health.Failed > 0 && strings.TrimSpace(health.LastError) != "" {
			fmt.Fprintf(&sb, "- last learning error: %s\n", truncate(toOneLine(tools.RedactSensitive(health.LastError)), 140))
		}
	}
	// Maintenance failure history (24h): the job row's last_error is
	// overwritten on every transition, so this append-only timeline is what
	// actually answers "why did learning fail" (e.g. deadline-exceeded runs
	// that were later skipped at the retry limit).
	if attempts, err := d.Control.RecentMaintenanceAttempts(ctx, identity.TenantID, time.Now().Add(-24*time.Hour), 20); err == nil && len(attempts) > 0 {
		byOutcome := map[string]int{}
		for _, a := range attempts {
			byOutcome[a.Outcome]++
		}
		fmt.Fprintf(&sb, "Learning failures (24h): failed %d, skipped %d, provider-blocked %d\n",
			byOutcome["failed"], byOutcome["skipped"], byOutcome["blocked_provider"])
		shown := 0
		for _, a := range attempts {
			if strings.TrimSpace(a.Error) == "" || shown >= 3 {
				continue
			}
			fmt.Fprintf(&sb, "- %s attempt %d (%s): %s\n", a.Outcome, a.Attempt,
				a.CreatedAt.Format("15:04"), truncate(toOneLine(tools.RedactSensitive(a.Error)), 120))
			shown++
		}
	}

	// Pending approvals.
	approvals, titles, err := d.pendingApprovalsForDisplay(ctx, identity)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&sb, "Pending approvals: %d\n", len(approvals))
	for _, approval := range approvals {
		fmt.Fprintf(&sb, "- %s (%s)\n", approvalSummaryLine(approval, titles[approval.TaskID]), approval.ID)
	}
	sb.WriteString(d.smartTriageDiagLines(ctx, identity))

	// Pending questions: a run blocked on a clarify is just as "stuck" as one
	// blocked on an approval, so it belongs in the same snapshot.
	clarifies, err := d.Control.ListClarifyRequests(ctx, identity.TenantID, identity.PersonID, "pending", 20)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&sb, "Pending questions: %d\n", len(clarifies))
	for _, clarify := range clarifies {
		fmt.Fprintf(&sb, "- %s (%s)\n", clarifySummaryLine(clarify), clarify.ID)
	}

	// Outbound delivery health (last 24h): sent / sent_unconfirmed /
	// pending_session / failed.
	// counts plus the newest undelivered reason, so "the push never reached my
	// phone" is diagnosable from the phone itself (P0-1).
	if counts, err := d.Control.CountOutboundByStatusSince(ctx, identity.TenantID, identity.PersonID, time.Now().Add(-24*time.Hour)); err == nil && len(counts) > 0 {
		fmt.Fprintf(&sb, "Outbound (24h): confirmed %d, accepted-unconfirmed %d, waiting-session %d, failed %d\n",
			counts["sent"], counts["sent_unconfirmed"], counts["pending_session"], counts["failed"])
		if counts["sent_unconfirmed"] > 0 || counts["pending_session"] > 0 || counts["failed"] > 0 {
			if undelivered, err := d.Control.ListUndeliveredOutbound(ctx, identity.TenantID, identity.PersonID, time.Now().Add(-24*time.Hour), 1); err == nil && len(undelivered) > 0 {
				u := undelivered[0]
				reason := strings.TrimSpace(u.LastError)
				if reason == "" && u.Status == "sent_unconfirmed" {
					reason = "platform accepted but delivery unconfirmed (stale session token; catch-up re-push arms on your next message)"
				}
				fmt.Fprintf(&sb, "- last undelivered: %s (%s)\n", truncate(toOneLine(tools.RedactSensitive(reason)), 140), u.Status)
			}
		}
	}
	if pending, err := d.Control.CountPendingSessionOutbound(ctx, identity.TenantID, identity.PersonID); err == nil && pending > 0 {
		fmt.Fprintf(&sb, "Pending session recovery: %d (use /diag delivery)\n", pending)
	}
	if health, err := d.Control.DeliveryHealthByPlatformSince(ctx, identity.TenantID, identity.PersonID, time.Now().Add(-24*time.Hour)); err == nil && len(health) > 0 {
		sb.WriteString("Delivery channels (24h):\n")
		for _, item := range health {
			fmt.Fprintf(&sb, "- %s: sent %d, unconfirmed %d, pending %d, failed %d\n",
				item.Platform, item.Sent, item.Unconfirmed, item.PendingSession, item.Failed)
		}
	}

	// Last run error across the person's recent runs.
	if runs, err := d.Control.ListRecentRunsForPerson(ctx, identity.TenantID, identity.PersonID, 10); err == nil {
		for _, r := range runs {
			if strings.TrimSpace(r.LastError) != "" {
				fmt.Fprintf(&sb, "Last error: %s\n", truncate(toOneLine(tools.RedactSensitive(r.LastError)), 160))
				break
			}
		}
	}

	// Recent events for the current task (fall back to the person's current
	// task pointer when no run is active).
	if currentTaskID == "" {
		if task, _ := d.Control.CurrentTask(ctx, identity.TenantID, identity.PersonID); task != nil {
			currentTaskID = task.ID
		}
	}
	if currentTaskID != "" {
		events, err := d.Control.ListTaskEvents(ctx, currentTaskID, diagRecentEvents)
		if err != nil {
			return "", err
		}
		// The breakdown is emitted at turn START, so it is usually pushed past
		// the recent-events window by later tool events — query a wider slice
		// just for it.
		if bdEvents, err := d.Control.ListTaskEvents(ctx, currentTaskID, 50); err == nil {
			if line := latestContextBreakdownLine(bdEvents); line != "" {
				sb.WriteString(line)
			}
		}
		if len(events) > 0 {
			sb.WriteString("Recent activity:\n")
			for _, event := range events {
				fmt.Fprintf(&sb, "- %s\n", diagEventLabel(event))
			}
		}
	}
	return strings.TrimSpace(sb.String()), nil
}

// deliveryDiagReply lists durable platform-session failures without exposing
// account or channel identifiers. A short delivery ID is sufficient for the
// scoped manual retry command in the same IM chat.
func (d *Server) deliveryDiagReply(ctx context.Context, identity *control.IdentityContext) (string, error) {
	if d == nil || d.Control == nil || identity == nil {
		return "Delivery diagnostics unavailable.", nil
	}
	rows, err := d.Control.ListPendingSessionOutbound(ctx, identity.TenantID, identity.PersonID, 5)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "Delivery recovery\nNo messages are waiting for a fresh IM session.", nil
	}
	total, _ := d.Control.CountPendingSessionOutbound(ctx, identity.TenantID, identity.PersonID)
	var sb strings.Builder
	fmt.Fprintf(&sb, "Delivery recovery\n%d message(s) are waiting for a fresh IM session.\n", total)
	if total > len(rows) {
		fmt.Fprintf(&sb, "Showing the oldest %d.\n", len(rows))
	}
	for _, row := range rows {
		ref := shortDeliveryID(row.ID)
		kind := strings.TrimSpace(row.Kind)
		if kind == "" {
			kind = "message"
		}
		age := time.Since(row.UpdatedAt)
		stale := d.Delivery == nil || age > d.Delivery.CatchUpMaxAge()
		state := "waiting for fresh session"
		if stale {
			state = "outside automatic recovery window"
		}
		fmt.Fprintf(&sb, "- %s | %s | %s | age %s\n", ref, kind, state, humanDuration(time.Since(row.CreatedAt)))
		fmt.Fprintf(&sb, "  Retry here once: /diag delivery retry %s\n", ref)
		fmt.Fprintf(&sb, "  Dismiss: /diag delivery dismiss %s\n", ref)
	}
	sb.WriteString("Automatic recovery only considers recent messages after a fresh inbound in the affected IM chat.")
	return strings.TrimSpace(sb.String()), nil
}

func (d *Server) dismissDeliveryReply(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, ref string) (string, error) {
	if d == nil || d.Delivery == nil || identity == nil {
		return "Delivery recovery unavailable.", nil
	}
	if strings.EqualFold(strings.TrimSpace(req.Platform), "cli") || strings.TrimSpace(req.Channel) == "" {
		return "Run this command in the affected IM chat.", nil
	}
	id, err := d.Delivery.DismissPendingSession(ctx, identity.TenantID, identity.PersonID, req.Platform, req.Channel, ref)
	if err != nil {
		return "No matching pending delivery was found in this IM chat.", nil
	}
	return "Dismissed delivery " + shortDeliveryID(id) + ".", nil
}

func (d *Server) retryDeliveryReply(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, ref string) (string, error) {
	if d == nil || d.Delivery == nil || identity == nil {
		return "Delivery recovery unavailable.", nil
	}
	if strings.EqualFold(strings.TrimSpace(req.Platform), "cli") || strings.TrimSpace(req.Channel) == "" {
		return "Run this recovery command in the affected IM chat.", nil
	}
	id, status, err := d.Delivery.RetryPendingSession(ctx, identity.TenantID, identity.PersonID, req.Platform, req.Channel, ref)
	shortID := shortDeliveryID(id)
	if err != nil {
		switch status {
		case "pending_session":
			return "The IM session is still unavailable. Send a fresh message here, then retry if needed.", nil
		case "failed":
			return "Delivery recovery failed permanently. Run /diag for the redacted reason.", nil
		default:
			return "No matching recoverable delivery was found in this IM chat.", nil
		}
	}
	switch status {
	case "sent":
		return "Recovered delivery " + shortID + ".", nil
	case "sent_unconfirmed":
		return "The platform accepted delivery " + shortID + ", but receipt is unconfirmed. It will not be retried automatically.", nil
	default:
		return "Delivery recovery finished with status: " + status, nil
	}
}

func shortDeliveryID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return "under a minute"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func (d *Server) memoryDiagReply(ctx context.Context, identity *control.IdentityContext) (string, error) {
	if d == nil || d.Gateway == nil || identity == nil {
		return "Memory diagnostics unavailable.", nil
	}
	result, err := d.Gateway.DispatchTool("memory", map[string]interface{}{
		"action": "stats", "_tenant_id": identity.PersonID,
	})
	if err != nil {
		return "", err
	}
	mode := "disabled"
	if d.MemoryConsolidator != nil {
		mode = d.MemoryConsolidator.Mode()
	}
	reply := strings.TrimSpace(result) + "\nGovernance mode: " + mode
	// Optional capability (W2): consolidation progress from the durable
	// judgement checkpoints, when the consolidator implements it.
	if summarizer, ok := d.MemoryConsolidator.(interface {
		PassSummary(ctx context.Context, personID string) string
	}); ok {
		if line := summarizer.PassSummary(ctx, identity.PersonID); line != "" {
			reply += "\n" + line
		}
	}
	return reply, nil
}

// contextDiagReply is /diag context: where the last turn's prompt went
// (expanded breakdown), what compaction bought, and what recall did — all
// read back from persisted run events, zero model tokens (W2).
func (d *Server) contextDiagReply(ctx context.Context, identity *control.IdentityContext) (string, error) {
	if d == nil || d.Control == nil || identity == nil {
		return "Context diagnostics unavailable.", nil
	}
	taskID := ""
	if active := d.coordinator().currentActive(identity.PersonID); active != nil {
		taskID = active.TaskID
	} else if task, _ := d.Control.CurrentTask(ctx, identity.TenantID, identity.PersonID); task != nil {
		taskID = task.ID
	}
	if taskID == "" {
		return "Context diagnostics\nNo current task — send a message first, then re-run /diag context.", nil
	}
	events, err := d.Control.ListTaskEvents(ctx, taskID, 120)
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	sb.WriteString("Context diagnostics (current task)\n")

	if line := contextBreakdownDetail(events); line != "" {
		sb.WriteString(line)
	} else {
		sb.WriteString("Breakdown: no context.breakdown event recorded yet\n")
	}
	sb.WriteString(latestProviderContextBreakdownLine(events))
	sb.WriteString(promptPrefixStabilityLine(events))
	sb.WriteString(promptCacheAggregateLine(events))
	sb.WriteString(latestPromptCacheLine(events))
	sb.WriteString(latestCompactionLine(events))
	sb.WriteString(latestRecallLine(events))
	return strings.TrimSpace(sb.String()), nil
}

func promptCacheAggregateLine(events []control.Event) string {
	var calls, hits, input, output, read, miss, created, reasoning int
	creationReported := false
	usageReported := false
	for _, e := range events {
		if e.Type != "provider.call.usage" {
			continue
		}
		var p struct {
			InputTokens           int  `json:"input_tokens"`
			OutputTokens          int  `json:"output_tokens"`
			CacheReadTokens       int  `json:"cache_read_input_tokens"`
			CacheMissTokens       int  `json:"cache_miss_input_tokens"`
			CacheCreationTokens   int  `json:"cache_creation_input_tokens"`
			ReasoningOutputTokens int  `json:"reasoning_output_tokens"`
			CacheUsageReported    bool `json:"cache_usage_reported"`
			CacheCreationReported bool `json:"cache_creation_reported"`
		}
		if json.Unmarshal(e.Payload, &p) != nil {
			continue
		}
		calls++
		input += p.InputTokens
		output += p.OutputTokens
		read += p.CacheReadTokens
		miss += p.CacheMissTokens
		created += p.CacheCreationTokens
		reasoning += p.ReasoningOutputTokens
		if p.CacheReadTokens > 0 {
			hits++
		}
		creationReported = creationReported || p.CacheCreationReported
		usageReported = usageReported || p.CacheUsageReported
	}
	if calls == 0 {
		return ""
	}
	hitRate := 0
	if input > 0 {
		hitRate = read * 100 / input
	}
	creation := "creation not reported by this transport"
	if creationReported {
		creation = fmt.Sprintf("created %d tok", created)
	}
	missText := fmt.Sprintf("uncached ~%d tok", max(input-read, 0))
	if usageReported {
		missText = fmt.Sprintf("cache miss %d tok", miss)
	}
	return fmt.Sprintf(
		"Prompt cache (visible %d calls): hit %d/%d tok (%d%%), %s, output %d tok (reasoning %d), hits %d/%d, %s\n",
		calls, read, input, hitRate, missText, output, reasoning, hits, calls, creation,
	)
}

func latestPromptCacheLine(events []control.Event) string {
	var callLine, runLine string
	for _, e := range events {
		var p struct {
			Iteration             int    `json:"iteration"`
			Transport             string `json:"transport"`
			Status                string `json:"status"`
			DurationMS            int64  `json:"duration_ms"`
			InputTokens           int    `json:"input_tokens"`
			OutputTokens          int    `json:"output_tokens"`
			CacheReadTokens       int    `json:"cache_read_input_tokens"`
			CacheMissTokens       int    `json:"cache_miss_input_tokens"`
			CacheCreationTokens   int    `json:"cache_creation_input_tokens"`
			ReasoningOutputTokens int    `json:"reasoning_output_tokens"`
			CacheUsageReported    bool   `json:"cache_usage_reported"`
			CacheCreationReported bool   `json:"cache_creation_reported"`
			UncachedInputTokens   int    `json:"uncached_input_tokens"`
		}
		if json.Unmarshal(e.Payload, &p) != nil {
			continue
		}
		hitRate := 0
		if p.InputTokens > 0 {
			hitRate = p.CacheReadTokens * 100 / p.InputTokens
		}
		created := "created n/a"
		if p.CacheCreationReported {
			created = fmt.Sprintf("created %d tok", p.CacheCreationTokens)
		}
		switch e.Type {
		case "provider.call.usage":
			if callLine == "" {
				callLine = fmt.Sprintf("Provider call (#%d %s, %s, %dms): input %d tok, cache hit %d tok (%d%%), cache miss %d tok, output %d tok (reasoning %d), %s\n",
					p.Iteration, p.Transport, p.Status, p.DurationMS, p.InputTokens,
					p.CacheReadTokens, hitRate, p.UncachedInputTokens, p.OutputTokens, p.ReasoningOutputTokens, created)
			}
		case "token.updated":
			if runLine == "" && (p.InputTokens > 0 || p.CacheReadTokens > 0 || p.CacheCreationTokens > 0) {
				runLine = fmt.Sprintf("Prompt cache (run snapshot): hit %d tok (%d%%), uncached %d tok, output %d tok (reasoning %d), %s\n",
					p.CacheReadTokens, hitRate, p.UncachedInputTokens, p.OutputTokens, p.ReasoningOutputTokens, created)
			}
		}
		if callLine != "" && runLine != "" {
			break
		}
	}
	if callLine != "" || runLine != "" {
		return callLine + runLine
	}
	return "Prompt cache: no provider usage data recorded yet\n"
}

func promptPrefixStabilityLine(events []control.Event) string {
	hashes := make([]string, 0, 2)
	for _, e := range events {
		if e.Type != "context.breakdown" {
			continue
		}
		var payload struct {
			StablePrefixHash string `json:"stable_prefix_hash"`
		}
		if json.Unmarshal(e.Payload, &payload) != nil || strings.TrimSpace(payload.StablePrefixHash) == "" {
			continue
		}
		hashes = append(hashes, payload.StablePrefixHash)
		if len(hashes) == 2 {
			break
		}
	}
	if len(hashes) == 0 {
		return "Prompt prefix: no fingerprint recorded yet\n"
	}
	if len(hashes) == 1 {
		return fmt.Sprintf("Prompt prefix: %s (need another turn to compare)\n", hashes[0])
	}
	if hashes[0] == hashes[1] {
		return fmt.Sprintf("Prompt prefix: stable across the last two turns (%s)\n", hashes[0])
	}
	return fmt.Sprintf("Prompt prefix: changed between the last two turns (%s -> %s)\n", hashes[1], hashes[0])
}

// contextBreakdownDetail is the multi-line expansion of the newest
// context.breakdown event (the /diag one-liner, one section per line).
func contextBreakdownDetail(events []control.Event) string {
	for _, e := range events {
		if e.Type != "context.breakdown" {
			continue
		}
		var raw struct {
			Identity         int    `json:"identity"`
			Tools            int    `json:"tools"`
			ProjectContext   int    `json:"project_context"`
			Memory           int    `json:"memory"`
			Runtime          int    `json:"runtime"`
			Recall           int    `json:"recall"`
			Artifacts        int    `json:"artifacts"`
			History          int    `json:"history"`
			ToolResults      int    `json:"tool_results"`
			Total            int    `json:"total"`
			Stable           int    `json:"stable"`
			Volatile         int    `json:"volatile"`
			StablePrefixHash string `json:"stable_prefix_hash"`
		}
		if json.Unmarshal(e.Payload, &raw) != nil || raw.Total <= 0 {
			return ""
		}
		total := raw.Total
		var sb strings.Builder
		fmt.Fprintf(&sb, "Breakdown (last turn, ~%d tok):\n", total)
		for _, section := range []struct {
			value int
			label string
		}{
			{raw.Identity, "identity/soul"},
			{raw.Tools, "tool contract"},
			{raw.ProjectContext, "project context (AGENTS.md)"},
			{raw.Memory, "person memory"},
			{raw.Runtime, "task/run context"},
			{raw.Recall, "semantic recall"},
			{raw.Artifacts, "artifact references"},
			{raw.History, "history"},
			{raw.ToolResults, "tool results"},
		} {
			fmt.Fprintf(&sb, "- %-38s ~%d tok (%d%%)\n", section.label, section.value, section.value*100/total)
		}
		// Assembly-time accounting (W5): the stable share is the cacheable
		// prompt prefix. Absent on events recorded before the accounting landed.
		if raw.Stable+raw.Volatile > 0 {
			fmt.Fprintf(&sb, "Prompt cache: ~%d tok stable prefix, ~%d tok volatile suffix", raw.Stable, raw.Volatile)
			if raw.StablePrefixHash != "" {
				fmt.Fprintf(&sb, " (prefix %s)", raw.StablePrefixHash)
			}
			sb.WriteString("\n")
		}
		return sb.String()
	}
	return ""
}

func latestProviderContextBreakdownLine(events []control.Event) string {
	for _, event := range events {
		if event.Type != "provider.call.context_breakdown" {
			continue
		}
		var p struct {
			Iteration          int    `json:"iteration"`
			Transport          string `json:"transport"`
			StableSystem       int    `json:"stable_system"`
			ToolSchemas        int    `json:"tool_schemas"`
			History            int    `json:"history"`
			CurrentToolResults int    `json:"current_tool_results"`
			Recall             int    `json:"recall"`
			Workspace          int    `json:"workspace"`
			Artifacts          int    `json:"artifacts"`
			Memory             int    `json:"memory"`
			TaskRuntime        int    `json:"task_runtime"`
			EstimatedTotal     int    `json:"estimated_total"`
		}
		if json.Unmarshal(event.Payload, &p) != nil {
			continue
		}
		return fmt.Sprintf(
			"Provider request (#%d %s, ~%d tok): stable system %d, tool schemas %d, history %d, tool results %d, workspace %d, task %d, recall %d, memory %d, artifacts %d\n",
			p.Iteration, p.Transport, p.EstimatedTotal, p.StableSystem, p.ToolSchemas,
			p.History, p.CurrentToolResults, p.Workspace, p.TaskRuntime, p.Recall, p.Memory, p.Artifacts,
		)
	}
	return "Provider request: no per-call context breakdown recorded yet\n"
}

// latestCompactionLine renders the newest context.compacted event, or an
// explicit "none" so the user can tell "never compacted" from "no data".
func latestCompactionLine(events []control.Event) string {
	for _, e := range events {
		if e.Type != "context.compacted" {
			continue
		}
		var p struct {
			BeforeTokens     int   `json:"before_tokens"`
			AfterTokens      int   `json:"after_tokens"`
			MessagesReplaced int   `json:"messages_replaced"`
			DurationMS       int64 `json:"duration_ms"`
		}
		if json.Unmarshal(e.Payload, &p) != nil || p.BeforeTokens <= 0 {
			continue
		}
		return fmt.Sprintf("Compaction (last): ~%d → ~%d tok, %d messages folded into one summary, %dms\n",
			p.BeforeTokens, p.AfterTokens, p.MessagesReplaced, p.DurationMS)
	}
	return "Compaction: not triggered in recent turns\n"
}

// latestRecallLine renders the newest context.recall event — hits, skip
// reason, and cost. Zero-hit turns emit too, so absence here means recall is
// not wired at all.
func latestRecallLine(events []control.Event) string {
	overlapLine := latestRecallUsageLine(events)
	for _, e := range events {
		if e.Type != "context.recall" {
			continue
		}
		var p struct {
			Candidates map[string]int `json:"candidates"`
			Sources    map[string]int `json:"sources"`
			Slices     int            `json:"slices"`
			Expanded   bool           `json:"expanded"`
			Skipped    string         `json:"skipped"`
			ElapsedMS  int64          `json:"elapsed_ms"`
		}
		if json.Unmarshal(e.Payload, &p) != nil {
			continue
		}
		if p.Skipped != "" {
			return fmt.Sprintf("Recall (last turn): skipped (%s)\n", p.Skipped) + overlapLine
		}
		parts := make([]string, 0, len(p.Sources))
		for name, count := range p.Sources {
			parts = append(parts, fmt.Sprintf("%s=%d", name, count))
		}
		sort.Strings(parts)
		src := strings.Join(parts, ", ")
		if src == "" {
			src = "no hits"
		}
		expanded := ""
		if p.Expanded {
			expanded = ", query expanded"
		}
		candidateParts := make([]string, 0, len(p.Candidates))
		for name, count := range p.Candidates {
			candidateParts = append(candidateParts, fmt.Sprintf("%s=%d", name, count))
		}
		sort.Strings(candidateParts)
		candidateText := strings.Join(candidateParts, ", ")
		if candidateText == "" {
			candidateText = "none"
		}
		return fmt.Sprintf("Recall (last turn): candidates [%s], selected %d slice(s) [%s]%s, %dms\n", candidateText, p.Slices, src, expanded, p.ElapsedMS) + overlapLine
	}
	return "Recall: no recall event recorded yet\n" + overlapLine
}

func latestRecallUsageLine(events []control.Event) string {
	for _, e := range events {
		if e.Type != "context.recall_usage" {
			continue
		}
		var p struct {
			Selected      int            `json:"selected"`
			OutputOverlap int            `json:"output_overlap"`
			Sources       map[string]int `json:"overlap_sources"`
			Method        string         `json:"method"`
		}
		if json.Unmarshal(e.Payload, &p) != nil {
			continue
		}
		parts := make([]string, 0, len(p.Sources))
		for source, count := range p.Sources {
			parts = append(parts, fmt.Sprintf("%s=%d", source, count))
		}
		sort.Strings(parts)
		detail := strings.Join(parts, ", ")
		if detail == "" {
			detail = "none"
		}
		return fmt.Sprintf("Recall output overlap: %d/%d slice(s) [%s] (trend signal, not causal proof)\n", p.OutputOverlap, p.Selected, detail)
	}
	return ""
}

// tasksDiagReply is /diag tasks: label hygiene counts plus a bounded
// "possibly stuck" list — open work whose last activity is old enough that
// the owner probably forgot it, never an automatic state change (W2).
func (d *Server) tasksDiagReply(ctx context.Context, identity *control.IdentityContext) (string, error) {
	if d == nil || d.Control == nil || identity == nil {
		return "Task diagnostics unavailable.", nil
	}
	var sb strings.Builder
	sb.WriteString("Task diagnostics\n")
	if stats, err := d.Control.ReadTaskGovernanceStats(ctx, identity.TenantID, identity.PersonID); err == nil {
		fmt.Fprintf(&sb, "Labels: open %d, terminal %d, archived %d, pinned %d, inbox runs %d\n",
			stats.Open, stats.Terminal, stats.Archived, stats.Pinned, stats.InboxRuns)
	}
	if stats, err := d.Control.ReadTaskReferenceStats(ctx, identity.TenantID, identity.PersonID); err == nil {
		fmt.Fprintf(&sb, "Task references: active %d, candidate %d, conflicted %d, shadow %d\n",
			stats.Active, stats.Candidate, stats.Conflicted, stats.Shadow)
		fmt.Fprintf(&sb, "Task routing audit: corrected %d, pending %d, unverified %d\n",
			stats.ResolutionCorrected, stats.ResolutionPending, stats.ResolutionUnverified)
		fmt.Fprintf(&sb, "Workspace knowledge: %d files, %d sections\n", stats.KnowledgeFiles, stats.KnowledgeSections)
	}
	queued, _ := d.Control.CountQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
	approvals, _, _ := d.pendingApprovalsForDisplay(ctx, identity)
	clarifies, _ := d.Control.ListClarifyRequests(ctx, identity.TenantID, identity.PersonID, "pending", 20)
	fmt.Fprintf(&sb, "Waiting: queued %d, pending approvals %d, pending questions %d\n", queued, len(approvals), len(clarifies))

	// Possibly-stuck scan over the most recent open cards (bounded, read-only).
	if cards, err := d.Control.ListTaskCards(ctx, identity.TenantID, identity.PersonID, 50); err == nil {
		lines := stuckTaskLines(cards, time.Now())
		if len(lines) == 0 {
			sb.WriteString("Possibly stuck: none\n")
		} else {
			fmt.Fprintf(&sb, "Possibly stuck (%d): oldest first — /resume to continue, /task <id> to inspect\n", len(lines))
			for _, line := range lines {
				sb.WriteString(line)
			}
		}
	}
	return strings.TrimSpace(sb.String()), nil
}

// Stuck thresholds: open work parked without a pending human decision for
// this long is probably forgotten, not in progress. Read-model only — the
// state machine is untouched (the stuck-run invariant belongs to recovery).
const (
	stuckInterruptedAfter = 48 * time.Hour
	stuckInProgressAfter  = 7 * 24 * time.Hour
)

// stuckTaskLines renders the possibly-stuck task list (≤5, oldest first)
// from task cards. Pure so the thresholds stay unit-testable.
func stuckTaskLines(cards []control.TaskCard, now time.Time) []string {
	type stuck struct {
		card control.TaskCard
		age  time.Duration
	}
	var found []stuck
	for _, card := range cards {
		status := strings.ToLower(strings.TrimSpace(card.Status))
		age := now.Sub(card.UpdatedAt)
		switch status {
		case "interrupted", api.RunStatusVerificationPartial, "blocked":
			if age > stuckInterruptedAfter {
				found = append(found, stuck{card: card, age: age})
			}
		case "in_progress":
			if age > stuckInProgressAfter {
				found = append(found, stuck{card: card, age: age})
			}
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].age > found[j].age })
	lines := make([]string, 0, 5)
	for i, item := range found {
		if i >= 5 {
			break
		}
		lines = append(lines, fmt.Sprintf("- [%s] %s (%s, idle %s)\n",
			item.card.Status, truncate(toOneLine(item.card.Title), 48), item.card.TaskID, formatIdleAge(item.age)))
	}
	return lines
}

func formatIdleAge(age time.Duration) string {
	if age >= 24*time.Hour {
		return fmt.Sprintf("%dd", int(age.Hours()/24))
	}
	return fmt.Sprintf("%dh", int(age.Hours()))
}

// diagEventLabel keeps /diag useful on IM without exposing internal event
// vocabulary or channel/account identifiers. Unknown events remain visible in
// a generic form so diagnostics do not silently hide activity.
func diagEventLabel(event control.Event) string {
	eventType := strings.TrimSpace(event.Type)
	if eventType == "run.finished" {
		var payload struct {
			Outcome struct {
				Status string `json:"status"`
			} `json:"outcome"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && strings.EqualFold(payload.Outcome.Status, "verification_partial") {
			return "Work changed; verification remains"
		}
	}
	switch eventType {
	case "run.started":
		return "Task started"
	case "run.finished", "turn.completed":
		return "Task completed"
	case "run.failed", "run.finalize_error":
		return "Task failed"
	case "run.interrupted":
		return "Task interrupted and resumable"
	case "run.recovery_notified":
		return "Recovery notice sent"
	case "maintenance.blocked":
		return "Background learning paused"
	case "external_watch.created":
		return "External watch started"
	case "external_watch.completed":
		return "External watch completed"
	case "agent.thinking", "strategy.selected", "context.selected", "context.recall", "context.breakdown":
		return "AI prepared the next step"
	case "tool.started":
		return "Tool started"
	case "tool.completed":
		return "Tool completed"
	case "artifact.created":
		return "Artifact created"
	case "approval.requested":
		return "Approval requested"
	case "approval.approved":
		return "Approval approved"
	case "approval.rejected":
		return "Approval rejected"
	case "token.updated":
		return "Channel session refreshed"
	default:
		return "Activity recorded"
	}
}

// latestContextBreakdownLine renders the newest context.breakdown event (P1-2)
// as a one-line share view — where the last turn's prompt tokens went. Empty
// when no breakdown event is present. ListTaskEvents returns newest-first.
func latestContextBreakdownLine(events []control.Event) string {
	for _, e := range events {
		if e.Type != "context.breakdown" {
			continue
		}
		var p struct {
			Identity, Tools, ProjectContext, Memory, Runtime, History, Total int
		}
		var raw map[string]int
		if json.Unmarshal(e.Payload, &raw) != nil {
			return ""
		}
		p.Identity, p.Tools, p.ProjectContext = raw["identity"], raw["tools"], raw["project_context"]
		p.Memory, p.Runtime, p.History, p.Total = raw["memory"], raw["runtime"], raw["history"], raw["total"]
		if p.Total <= 0 {
			return ""
		}
		pct := func(n int) int { return n * 100 / p.Total }
		return fmt.Sprintf(
			"Context (last turn, ~%d tok): identity %d%%, tools %d%%, project %d%%, memory %d%%, runtime %d%%, history %d%%\n",
			p.Total, pct(p.Identity), pct(p.Tools), pct(p.ProjectContext), pct(p.Memory), pct(p.Runtime), pct(p.History))
	}
	return ""
}

// scratchQuotaNote reports the soft-limit state using the execution layer's
// single constant, so the diagnostic and the enforcement can never disagree.
func scratchQuotaNote(bytes int64) string {
	if bytes < tools.ScratchQuotaSoftLimitBytes {
		return ""
	}
	return " (over the 2 GiB soft limit; the next command is blocked until it is cleaned up)"
}

func compactBytes(bytes int64) string {
	switch {
	case bytes >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(bytes)/float64(1<<30))
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(bytes)/float64(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(bytes)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

func compactDuration(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%.1fh", d.Hours())
	case d >= time.Minute:
		return fmt.Sprintf("%.0fm", d.Minutes())
	default:
		return fmt.Sprintf("%.0fs", d.Seconds())
	}
}

// recentExecutionSummary reports the execution evidence the metrics in
// docs/execution-engine.zh-CN.md depend on: which profiles were applied, how
// recovery went, and why any command left the sandbox. Without the host-escape
// REASON the only measurable number is the raw host share, which mixes a
// deliberate login with a sandbox defect.
func (d *Server) recentExecutionSummary(ctx context.Context, tenantID, taskID, runID string) string {
	_ = tenantID
	if d == nil || d.Control == nil || strings.TrimSpace(taskID) == "" {
		return ""
	}
	events, err := d.Control.ListTaskEvents(ctx, taskID, 200)
	if err != nil {
		return ""
	}
	profiles := map[string]bool{}
	notes := 0
	recoveries := 0
	escapes := map[string]int{}
	for _, event := range events {
		if strings.TrimSpace(runID) != "" && event.RunID != runID {
			continue
		}
		switch event.Type {
		case "tool.environment":
			var payload struct {
				Profiles []string `json:"profiles"`
				Notes    []string `json:"notes"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil {
				for _, id := range payload.Profiles {
					profiles[id] = true
				}
				notes += len(payload.Notes)
			}
		case "tool.recovery":
			recoveries++
		case "tool.sandbox":
			var payload struct {
				Mode             string `json:"mode"`
				HostEscapeReason string `json:"host_escape_reason"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.Mode == "host" {
				escapes[fallback(payload.HostEscapeReason, "unclassified")]++
			}
		}
	}
	if len(profiles) == 0 && recoveries == 0 && len(escapes) == 0 {
		return ""
	}
	var sb strings.Builder
	if len(profiles) > 0 {
		names := make([]string, 0, len(profiles))
		for id := range profiles {
			names = append(names, id)
		}
		sort.Strings(names)
		fmt.Fprintf(&sb, "Environment profiles applied this run: %s\n", strings.Join(names, ", "))
	}
	if notes > 0 {
		fmt.Fprintf(&sb, "Environment notes this run: %d (see run events)\n", notes)
	}
	if recoveries > 0 {
		fmt.Fprintf(&sb, "Environment recoveries this run: %d\n", recoveries)
	}
	if len(escapes) > 0 {
		reasons := make([]string, 0, len(escapes))
		for reason, count := range escapes {
			reasons = append(reasons, fmt.Sprintf("%s=%d", reason, count))
		}
		sort.Strings(reasons)
		fmt.Fprintf(&sb, "Host escapes this run: %s\n", strings.Join(reasons, ", "))
	}
	return sb.String()
}
