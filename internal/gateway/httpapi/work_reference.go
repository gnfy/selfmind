package httpapi

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

// Attention reference resolution shared by the control commands.
//
// This file used to render the /tasks card view and the /task subcommands.
// Both are gone: Attention derives per exact Run, so "what needs me" is a run
// listing (attention_list.go) and there is no Task view left to manage. What
// survives is reference resolution — turning a number, a run id, or a thread
// id copied out of an older transcript into the thing the person meant.

// tasksAttentionScanLimit bounds the one ranked Attention read a --workspace
// filter pages in memory, because the workspace is not part of the ranked
// Attention query.
const tasksAttentionScanLimit = 500

// explicitResumeRunStatus reports whether a Run parked in this status is
// continued by an explicit /resume. Monitoring (waiting_external) and
// executing (running) Runs are Attention too, but nothing about them is the
// person's to resume, so their cards carry no resume line.
func explicitResumeRunStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "interrupted", "waiting_user", api.RunStatusVerificationPartial, "blocked":
		return true
	}
	return false
}

// attentionDismissalRefusal maps the control store's dismissal refusals to the
// sentence the person sees. Both are decisions, not failures: the person must
// stop the run, or answer, reject, or cancel the control object first.
func attentionDismissalRefusal(err error) (string, bool) {
	switch {
	case errors.Is(err, control.ErrAttentionPendingControl):
		return "This run still has a pending approval, clarification, or watcher. Answer, reject, or cancel it first; attention is not dismissed.", true
	case errors.Is(err, control.ErrTaskHasLiveWork):
		return "This thread has a run executing right now. Use /stop and wait for it to finish before dismissing its attention.", true
	}
	return "", false
}

// openAttentionPage returns exactly the Attention items one /tasks page renders
// plus the person's true Attention total. Without a workspace filter both come
// from the database, so work past one page stays reachable and countable; a
// workspace is not part of the ranked Attention query, so that variant ranks
// one bounded read and pages the filtered items.
func (d *Server) openAttentionPage(ctx context.Context, identity *control.IdentityContext, workspace, preferChannel string, limit, offset int) ([]control.AttentionItem, int, error) {
	timeline := control.NewWorkTimeline(d.Control)
	if workspace == "" {
		return timeline.AttentionPage(ctx, identity.TenantID, identity.PersonID, preferChannel, limit, offset)
	}
	scanned, err := timeline.AttentionForChannel(ctx, identity.TenantID, identity.PersonID, preferChannel, tasksAttentionScanLimit)
	if err != nil {
		return nil, 0, err
	}
	filtered := scanned[:0]
	for _, item := range scanned {
		if item.Thread.WorkspaceID == workspace {
			filtered = append(filtered, item)
		}
	}
	return attentionPage(filtered, limit, offset), len(filtered), nil
}

func attentionPage(items []control.AttentionItem, limit, offset int) []control.AttentionItem {
	if offset >= len(items) {
		return nil
	}
	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
	return items[offset:end]
}

// truncateRunes bounds display text by RUNE count (CJK titles get the same
// visible width budget as ASCII; textutil.Truncate is byte-based).
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if max <= 0 || len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}

// listTasksForDisplay fetches the default ranked open cards. It is only the
// fallback for an ordinal used without a recent endpoint snapshot; ordinary
// /tasks -> /task or /resume flows resolve the exact rendered snapshot.
func (d *Server) listTasksForDisplay(ctx context.Context, identity *control.IdentityContext) ([]control.Task, error) {
	page, err := d.Control.QueryTasks(ctx, identity.TenantID, identity.PersonID, control.TaskQuery{
		View: "open", Limit: d.TaskGovernance.listLimit(),
	})
	if err != nil {
		return nil, err
	}
	return page.Tasks, nil
}

// resolveTaskReference resolves a user-supplied task reference for control
// commands, mirroring the approval resolver contract (approval_resolver.go):
// a bare number is a LIST ORDINAL against the numbered cards of the default
// /tasks view, and anything else is a full id or unique short prefix
// (findTaskByRef; the card-displayed
// task_xxxxxxxx form round-trips). Shared by /task and /resume so the same
// reference means the same task on every surface.
//
// The middle return is a user-facing sentence (safe to send verbatim on any
// channel) for reference mistakes; the error return is reserved for storage
// failures.
func (d *Server) resolveTaskReference(ctx context.Context, identity *control.IdentityContext, ref string, channels ...string) (*control.Task, string, error) {
	ref = strings.TrimSpace(ref)
	if ordinal, convErr := strconv.Atoi(ref); convErr == nil {
		if taskID, runID, count, found := d.taskLists.resolveRun(identity, firstString(channels), ordinal, time.Now()); found {
			if taskID == "" {
				return nil, fmt.Sprintf("No task number %d in the last list; it showed %d (run /tasks to refresh).", ordinal, count), nil
			}
			task, err := d.findTaskByRef(ctx, identity, taskID)
			if err != nil {
				return nil, "", err
			}
			if task == nil {
				return nil, "That numbered task is no longer available. Run /tasks to refresh the list.", nil
			}
			task.ResumeRunID = runID
			return task, "", nil
		}
		tasks, err := d.listTasksForDisplay(ctx, identity)
		if err != nil {
			return nil, "", err
		}
		if len(tasks) == 0 {
			return nil, "Nothing currently needs attention; see /tasks or use a thread id.", nil
		}
		if ordinal < 1 || ordinal > len(tasks) {
			return nil, fmt.Sprintf("No attention item number %d; %d shown (see /tasks).", ordinal, len(tasks)), nil
		}
		d.taskLists.remember(identity, firstString(channels), tasks, time.Now())
		return &tasks[ordinal-1], "", nil
	}
	task, err := d.findTaskByRef(ctx, identity, ref)
	if err != nil {
		return nil, "", err
	}
	if task == nil {
		return nil, "Task not found. Run /tasks to list ids.", nil
	}
	return task, "", nil
}

// findTaskByRef resolves a task reference for control commands: the full id,
// or a unique prefix (the short `task_xxxxxxxx` form the aggregated view
// prints). Only the person's own tasks resolve. Ordinal references resolve in
// resolveTaskReference, which wraps this.
func (d *Server) findTaskByRef(ctx context.Context, identity *control.IdentityContext, ref string) (*control.Task, error) {
	ref = strings.TrimSpace(ref)
	// A card id copied verbatim ends in "..." (or a unicode ellipsis); treat it
	// as the prefix it abbreviates.
	ref = strings.TrimRight(ref, ".…")
	if ref == "" {
		return nil, nil
	}
	task, err := d.Control.GetTask(ctx, identity.TenantID, ref)
	if err != nil {
		return nil, err
	}
	if task != nil {
		if task.PersonID != identity.PersonID {
			return nil, nil
		}
		return task, nil
	}
	// Prefix match — require at least the short-id length so a bare "task_"
	// can never resolve, and refuse ambiguity.
	if len(ref) < len("task_")+8 {
		return nil, nil
	}
	tasks, err := d.Control.ListTasks(ctx, identity.TenantID, identity.PersonID, 200)
	if err != nil {
		return nil, err
	}
	var match *control.Task
	for i := range tasks {
		if strings.HasPrefix(tasks[i].ID, ref) {
			if match != nil {
				return nil, nil // ambiguous
			}
			match = &tasks[i]
		}
	}
	return match, nil
}

// shortTaskID renders the compact reference form `task_xxxxxxxx` used in
// hints (/resume task_xxxxxxxx); findTaskByRef resolves it back.
func shortTaskID(id string) string {
	const prefix = "task_"
	if strings.HasPrefix(id, prefix) && len(id) > len(prefix)+8 {
		return id[:len(prefix)+8]
	}
	return id
}

// humanAge renders a compact relative age for list lines.
func humanAge(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
