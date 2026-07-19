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
	"selfmind/internal/tools"
)

const diagRecentEvents = 5

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
	if watches, err := d.Control.CountExternalWatchesByStatus(ctx, identity.TenantID, identity.PersonID); err == nil {
		activeWatches := watches[control.ExternalWatchPending] + watches[control.ExternalWatchRunning]
		if activeWatches+watches[control.ExternalWatchFailed]+watches[control.ExternalWatchTimedOut] > 0 {
			fmt.Fprintf(&sb, "External watches: active %d, failed %d, timed out %d\n",
				activeWatches, watches[control.ExternalWatchFailed], watches[control.ExternalWatchTimedOut])
		}
		// A timed_out watch whose recorded output already matches a declared
		// terminal pattern is a misjudgment the startup recovery pass will
		// revise — surface it instead of leaving a silently wrong verdict.
		if watches[control.ExternalWatchTimedOut] > 0 {
			if finished, err := d.Control.ListExternalWatchesFinishedSince(ctx, control.ExternalWatchTimedOut, time.Now().Add(-externalWatchRecoveryLookback), 10); err == nil {
				for _, watch := range finished {
					if status := classifyExternalWatchOutput(watch, watch.LastOutput); status != "" {
						fmt.Fprintf(&sb, "- watch verdict suspect: %s timed out but last output matches %s (%s)\n",
							truncate(toOneLine(watch.Description), 60), status,
							truncate(toOneLine(strings.TrimSpace(watch.LastOutput)), 40))
					}
				}
			}
		}
	}
	if health, err := d.Control.MaintenanceHealthForPerson(ctx, identity.TenantID, identity.PersonID); err == nil {
		if health.Pending > 0 || health.Running > 0 {
			oldest := ""
			if !health.OldestPendingAt.IsZero() {
				oldest = fmt.Sprintf(", oldest %s", time.Since(health.OldestPendingAt).Round(time.Second))
			}
			fmt.Fprintf(&sb, "Background learning: queued %d, running %d (batched%s)\n", health.Pending, health.Running, oldest)
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

	// Outbound delivery health (last 24h): sent / sent_unconfirmed / failed
	// counts plus the newest undelivered reason, so "the push never reached my
	// phone" is diagnosable from the phone itself (P0-1).
	if counts, err := d.Control.CountOutboundByStatusSince(ctx, identity.TenantID, identity.PersonID, time.Now().Add(-24*time.Hour)); err == nil && len(counts) > 0 {
		fmt.Fprintf(&sb, "Outbound (24h): sent %d, unconfirmed %d, failed %d\n",
			counts["sent"], counts["sent_unconfirmed"], counts["failed"])
		if counts["sent_unconfirmed"] > 0 || counts["failed"] > 0 {
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
				fmt.Fprintf(&sb, "- %s\n", diagEventLabel(event.Type))
			}
		}
	}
	return strings.TrimSpace(sb.String()), nil
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
	sb.WriteString(latestPromptCacheLine(events))
	sb.WriteString(latestCompactionLine(events))
	sb.WriteString(latestRecallLine(events))
	return strings.TrimSpace(sb.String()), nil
}

func latestPromptCacheLine(events []control.Event) string {
	for _, e := range events {
		if e.Type != "token.updated" {
			continue
		}
		var p struct {
			InputTokens         int `json:"input_tokens"`
			CacheReadTokens     int `json:"cache_read_input_tokens"`
			CacheCreationTokens int `json:"cache_creation_input_tokens"`
			BilledInputTokens   int `json:"billed_input_tokens"`
		}
		if json.Unmarshal(e.Payload, &p) != nil {
			continue
		}
		if p.InputTokens <= 0 && p.CacheReadTokens <= 0 && p.CacheCreationTokens <= 0 {
			continue
		}
		hitRate := 0
		if p.InputTokens > 0 {
			hitRate = p.CacheReadTokens * 100 / p.InputTokens
		}
		return fmt.Sprintf("Prompt cache (run): read %d tok (%d%%), created %d tok, billed input %d tok\n",
			p.CacheReadTokens, hitRate, p.CacheCreationTokens, p.BilledInputTokens)
	}
	return "Prompt cache: no provider usage data recorded yet\n"
}

// contextBreakdownDetail is the multi-line expansion of the newest
// context.breakdown event (the /diag one-liner, one section per line).
func contextBreakdownDetail(events []control.Event) string {
	for _, e := range events {
		if e.Type != "context.breakdown" {
			continue
		}
		var raw map[string]int
		if json.Unmarshal(e.Payload, &raw) != nil || raw["total"] <= 0 {
			return ""
		}
		total := raw["total"]
		var sb strings.Builder
		fmt.Fprintf(&sb, "Breakdown (last turn, ~%d tok):\n", total)
		for _, section := range []struct{ key, label string }{
			{"identity", "identity/soul"},
			{"tools", "tool contract"},
			{"project_context", "project context (AGENTS.md)"},
			{"memory", "person memory"},
			{"runtime", "runtime context (task/workspace/recall)"},
			{"history", "history"},
		} {
			fmt.Fprintf(&sb, "- %-38s ~%d tok (%d%%)\n", section.label, raw[section.key], raw[section.key]*100/total)
		}
		// Assembly-time accounting (W5): the stable share is the cacheable
		// prompt prefix. Absent on events recorded before the accounting landed.
		if stable, ok := raw["stable"]; ok && stable+raw["volatile"] > 0 {
			fmt.Fprintf(&sb, "Prompt cache: ~%d tok stable prefix, ~%d tok volatile suffix\n", stable, raw["volatile"])
		}
		return sb.String()
	}
	return ""
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
	for _, e := range events {
		if e.Type != "context.recall" {
			continue
		}
		var p struct {
			Sources   map[string]int `json:"sources"`
			Slices    int            `json:"slices"`
			Expanded  bool           `json:"expanded"`
			Skipped   string         `json:"skipped"`
			ElapsedMS int64          `json:"elapsed_ms"`
		}
		if json.Unmarshal(e.Payload, &p) != nil {
			continue
		}
		if p.Skipped != "" {
			return fmt.Sprintf("Recall (last turn): skipped (%s)\n", p.Skipped)
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
		return fmt.Sprintf("Recall (last turn): %d slice(s) [%s]%s, %dms\n", p.Slices, src, expanded, p.ElapsedMS)
	}
	return "Recall: no recall event recorded yet\n"
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
		case "interrupted", "blocked":
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
func diagEventLabel(eventType string) string {
	switch strings.TrimSpace(eventType) {
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
