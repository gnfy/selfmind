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
