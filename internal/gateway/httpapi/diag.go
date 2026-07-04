package httpapi

// /diag — the compact, from-the-phone runtime snapshot (observability, self-
// serve diagnostics). It answers "what is SelfMind doing right now and is
// anything stuck?" without the full `selfmind doctor` bundle: active run +
// elapsed, queued count, pending approvals, the last run error if any, and the
// current task's recent events. English, redacted, no raw hashes beyond the
// actionable approval ids that /approve already uses.

import (
	"context"
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

	// Pending approvals.
	approvals, titles, err := d.pendingApprovalsForDisplay(ctx, identity)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&sb, "Pending approvals: %d\n", len(approvals))
	for _, approval := range approvals {
		fmt.Fprintf(&sb, "- %s (%s)\n", approvalSummaryLine(approval, titles[approval.TaskID]), approval.ID)
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
		if len(events) > 0 {
			sb.WriteString("Recent events:\n")
			for _, event := range events {
				line := event.Type
				if event.Channel != "" {
					line += " [" + event.Channel + "]"
				}
				fmt.Fprintf(&sb, "- %s\n", line)
			}
		}
	}
	return strings.TrimSpace(sb.String()), nil
}
