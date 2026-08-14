package httpapi

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/platform/textutil"
	"selfmind/internal/tools"
)

const (
	watcherListPageSize = 8
	watcherUsage        = "Usage: /watchers [active|attention|recent|all [page]|<n|id>|cancel <n|id>]"
)

func (d *Server) watchersCommandReply(ctx context.Context, identity *control.IdentityContext, args []string) (string, error) {
	if d == nil || d.Control == nil || identity == nil {
		return "Watchers unavailable.", nil
	}
	if len(args) > 0 && strings.EqualFold(args[0], "cancel") {
		if len(args) != 2 {
			return watcherUsage, nil
		}
		return d.cancelWatcherReply(ctx, identity, args[1])
	}

	mode := control.ExternalWatchListSummary
	page := 1
	if len(args) > 0 {
		switch strings.ToLower(strings.TrimSpace(args[0])) {
		case control.ExternalWatchListActive, control.ExternalWatchListAttention,
			control.ExternalWatchListRecent, control.ExternalWatchListAll:
			mode = strings.ToLower(strings.TrimSpace(args[0]))
			if len(args) > 1 {
				parsed, err := strconv.Atoi(args[1])
				if err != nil || parsed < 1 || len(args) > 2 {
					return watcherUsage, nil
				}
				page = parsed
			}
		case "summary":
			if len(args) != 1 {
				return watcherUsage, nil
			}
		default:
			if len(args) != 1 {
				return watcherUsage, nil
			}
			return d.watcherDetailReply(ctx, identity, args[0])
		}
	}

	watches, err := d.Control.ListExternalWatchesForPerson(ctx, identity.TenantID, identity.PersonID,
		mode, watcherListPageSize, (page-1)*watcherListPageSize)
	if err != nil {
		return "", err
	}
	counts, err := d.Control.CountExternalWatchesByStatus(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		return "", err
	}
	return d.formatWatchersOverview(ctx, identity, watches, counts, mode, page), nil
}

func (d *Server) watcherDetailReply(ctx context.Context, identity *control.IdentityContext, ref string) (string, error) {
	watch, userErr, err := d.resolveWatcherReference(ctx, identity, ref)
	if err != nil {
		return "", err
	}
	if userErr != "" {
		return userErr, nil
	}

	task, workspace := d.watcherDisplayContext(ctx, identity, *watch)
	var sb strings.Builder
	fmt.Fprintf(&sb, "Watcher %s\n", shortExternalWatchID(watch.ID))
	fmt.Fprintf(&sb, "Status: %s\n", watch.Status)
	fmt.Fprintf(&sb, "Description: %s\n", watcherDescription(*watch))
	if task != nil {
		fmt.Fprintf(&sb, "Task: %s (%s)\n", textutil.Truncate(toOneLine(task.Title), 54), shortTaskID(task.ID))
		fmt.Fprintf(&sb, "Task status: %s\n", task.Status)
	}
	if workspace != nil {
		fmt.Fprintf(&sb, "Workspace: %s\n", textutil.Truncate(toOneLine(workspace.Name), 60))
	}
	fmt.Fprintf(&sb, "Phases: check %s | operation %s | verify %s\n",
		watcherPhase(watch.CheckerStatus, "pending"), watcherPhase(watch.OperationStatus, "pending"),
		watcherPhase(watch.VerificationStatus, "not_required"))
	fmt.Fprintf(&sb, "Attempts: %d | Created: %s | Updated: %s\n",
		watch.Attempts, humanAge(watch.CreatedAt), humanAge(watch.UpdatedAt))
	if externalWatchIsActive(watch.Status) {
		fmt.Fprintf(&sb, "Next check: %s | Deadline: %s\n",
			watcherRelativeFuture(watch.NextCheckAt), watcherRelativeFuture(watch.TimeoutAt))
	}
	fmt.Fprintf(&sb, "Finalized: %s | Notification: %s\n",
		yesNo(watch.Finalized), watcherNotificationState(*watch))
	if output := watcherSafeLine(watch.LastOutput, 160); output != "" {
		fmt.Fprintf(&sb, "Last result: %s\n", output)
	}
	if lastError := watcherSafeLine(watch.LastError, 160); lastError != "" {
		fmt.Fprintf(&sb, "Last error: %s\n", lastError)
	}
	if externalWatchIsActive(watch.Status) {
		fmt.Fprintf(&sb, "Cancel: /watchers cancel %s", shortExternalWatchID(watch.ID))
	}
	return strings.TrimSpace(sb.String()), nil
}

func (d *Server) cancelWatcherReply(ctx context.Context, identity *control.IdentityContext, ref string) (string, error) {
	watch, userErr, err := d.resolveWatcherReference(ctx, identity, ref)
	if err != nil {
		return "", err
	}
	if userErr != "" {
		return userErr, nil
	}
	if !externalWatchIsActive(watch.Status) {
		return fmt.Sprintf("Watcher %s is already %s.", shortExternalWatchID(watch.ID), watch.Status), nil
	}
	cancelled, err := d.Control.CancelExternalWatchForPerson(ctx, identity.TenantID, identity.PersonID, watch.ID)
	if err != nil {
		return "", err
	}
	if !cancelled {
		current, _, resolveErr := d.Control.ResolveExternalWatchForPerson(ctx, identity.TenantID, identity.PersonID, watch.ID)
		if resolveErr != nil {
			return "", resolveErr
		}
		if current != nil {
			return fmt.Sprintf("Watcher %s is already %s.", shortExternalWatchID(current.ID), current.Status), nil
		}
		return "Watcher changed before it could be cancelled. Run /watchers to refresh.", nil
	}
	_, _ = d.Control.AppendEvent(ctx, control.Event{
		TaskID: watch.TaskID, RunID: watch.RunID, Type: "external_watch.cancelled", Visibility: "task",
		Payload: mustJSON(map[string]string{"watch_id": watch.ID, "source": "user"}),
	})
	return fmt.Sprintf("Cancelled watcher %s. The external operation was not cancelled.\nResume the task to continue or register a new watcher.",
		shortExternalWatchID(watch.ID)), nil
}

// resolveWatcherReference mirrors the numbered /tasks contract: a bare number
// is the ordinal from the stable default/all watcher ordering, while any other
// token is a full id or unique displayed prefix. Filtered views deliberately
// stay unnumbered because their local "1" would identify a different watcher.
func (d *Server) resolveWatcherReference(ctx context.Context, identity *control.IdentityContext, ref string) (*control.ExternalWatch, string, error) {
	ref = strings.TrimSpace(ref)
	if ordinal, convErr := strconv.Atoi(ref); convErr == nil {
		if ordinal < 1 {
			return nil, fmt.Sprintf("No watcher number %d; run /watchers to see numbered watchers.", ordinal), nil
		}
		watches, err := d.Control.ListExternalWatchesForPerson(ctx, identity.TenantID, identity.PersonID,
			control.ExternalWatchListAll, 1, ordinal-1)
		if err != nil {
			return nil, "", err
		}
		if len(watches) == 0 {
			return nil, fmt.Sprintf("No watcher number %d; run /watchers all to see numbered watchers.", ordinal), nil
		}
		return &watches[0], "", nil
	}

	watch, ambiguous, err := d.Control.ResolveExternalWatchForPerson(ctx, identity.TenantID, identity.PersonID, ref)
	if err != nil {
		return nil, "", err
	}
	if ambiguous {
		return nil, "Watcher reference is ambiguous. Add more characters from /watchers.", nil
	}
	if watch == nil {
		return nil, "Watcher not found. Run /watchers all to see your watchers.", nil
	}
	return watch, "", nil
}

func (d *Server) formatWatchersOverview(ctx context.Context, identity *control.IdentityContext, watches []control.ExternalWatch, counts map[string]int, mode string, page int) string {
	active := counts[control.ExternalWatchPending] + counts[control.ExternalWatchRunning]
	attention := counts[control.ExternalWatchFailed] + counts[control.ExternalWatchTimedOut] + counts[control.ExternalWatchBlocked]
	var sb strings.Builder
	sb.WriteString("Watchers\n")
	fmt.Fprintf(&sb, "Active %d | Attention %d | Succeeded %d | Cancelled %d\n",
		active, attention, counts[control.ExternalWatchSucceeded], counts[control.ExternalWatchCancelled])
	if len(watches) == 0 {
		if mode == control.ExternalWatchListSummary && active+attention == 0 && counts[control.ExternalWatchSucceeded]+counts[control.ExternalWatchCancelled] == 0 {
			sb.WriteString("\nNo watchers yet.")
		} else {
			sb.WriteString("\nNo watchers match this view.")
		}
		return sb.String()
	}
	if page > 1 || mode == control.ExternalWatchListAll {
		fmt.Fprintf(&sb, "View: %s | Page %d\n", mode, page)
	}
	numbered := mode == control.ExternalWatchListSummary || mode == control.ExternalWatchListAll
	for i, watch := range watches {
		task, _ := d.watcherDisplayContext(ctx, identity, watch)
		sb.WriteString("\n")
		if numbered {
			fmt.Fprintf(&sb, "%d. [%s] %s\n", (page-1)*watcherListPageSize+i+1,
				strings.ToLower(watch.Status), watcherDescription(watch))
		} else {
			fmt.Fprintf(&sb, "- [%s] %s\n", strings.ToLower(watch.Status), watcherDescription(watch))
		}
		if task != nil {
			fmt.Fprintf(&sb, "   task: %s | %s\n", shortTaskID(task.ID), task.Status)
		}
		fmt.Fprintf(&sb, "   phases: check %s | operation %s | verify %s\n",
			watcherPhase(watch.CheckerStatus, "pending"), watcherPhase(watch.OperationStatus, "pending"),
			watcherPhase(watch.VerificationStatus, "not_required"))
		if externalWatchIsActive(watch.Status) {
			fmt.Fprintf(&sb, "   next: %s | deadline %s | attempts %d\n",
				watcherRelativeFuture(watch.NextCheckAt), watcherRelativeFuture(watch.TimeoutAt), watch.Attempts)
		} else {
			fmt.Fprintf(&sb, "   finished: %s | finalized %s | notify %s\n",
				watcherFinishedAge(watch), yesNo(watch.Finalized), watcherNotificationState(watch))
		}
		fmt.Fprintf(&sb, "   id: %s\n", shortExternalWatchID(watch.ID))
	}
	if numbered {
		sb.WriteString("\nOpen: /watchers <n|id> | Cancel: /watchers cancel <n|id>")
	} else {
		sb.WriteString("\nOpen: /watchers <id> | Cancel: /watchers cancel <id>")
	}
	if len(watches) == watcherListPageSize {
		nextMode := mode
		if mode == control.ExternalWatchListSummary {
			nextMode = control.ExternalWatchListAll
		}
		fmt.Fprintf(&sb, " | Next: /watchers %s %d", nextMode, page+1)
	}
	return strings.TrimSpace(sb.String())
}

func (d *Server) watcherDisplayContext(ctx context.Context, identity *control.IdentityContext, watch control.ExternalWatch) (*control.Task, *control.Workspace) {
	var task *control.Task
	if candidate, err := d.Control.GetTask(ctx, identity.TenantID, watch.TaskID); err == nil && candidate != nil && candidate.PersonID == identity.PersonID {
		task = candidate
	}
	var workspace *control.Workspace
	if watch.WorkspaceID != "" {
		if candidate, err := d.Control.GetWorkspace(ctx, identity.TenantID, watch.WorkspaceID); err == nil && candidate != nil && candidate.OwnerPersonID == identity.PersonID {
			workspace = candidate
		}
	}
	return task, workspace
}

func shortExternalWatchID(id string) string {
	id = strings.TrimSpace(id)
	const prefix = "watch_"
	if strings.HasPrefix(id, prefix) && len(id) > len(prefix)+8 {
		return id[:len(prefix)+8]
	}
	return id
}

func watcherDescription(watch control.ExternalWatch) string {
	description := strings.TrimSpace(watch.Description)
	if description == "" {
		description = "External operation"
	}
	return textutil.Truncate(toOneLine(tools.RedactSensitive(description)), 68)
}

func watcherSafeLine(value string, limit int) string {
	return textutil.Truncate(toOneLine(tools.RedactSensitive(strings.TrimSpace(value))), limit)
}

func watcherPhase(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func externalWatchIsActive(status string) bool {
	return status == control.ExternalWatchPending || status == control.ExternalWatchRunning
}

func watcherRelativeFuture(value time.Time) string {
	if value.IsZero() {
		return "unknown"
	}
	d := time.Until(value).Round(time.Second)
	if d <= 0 {
		return "due"
	}
	return "in " + compactDuration(d)
}

func watcherFinishedAge(watch control.ExternalWatch) string {
	if watch.FinishedAt != nil {
		return humanAge(*watch.FinishedAt)
	}
	return humanAge(watch.UpdatedAt)
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func watcherNotificationState(watch control.ExternalWatch) string {
	if watch.Notified {
		return "recorded"
	}
	return "pending"
}
