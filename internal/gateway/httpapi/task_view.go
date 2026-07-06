package httpapi

import (
	"context"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/platform/textutil"
)

// /tasks aggregated view + /task <id> subcommands (Work Timeline P3,
// docs/work-timeline.md "/tasks view"). Tasks are labels: the default view
// shows only OPEN work (one line per label with run count, age, workspace and
// a next-step hint), collapses finished work to a count, and /task <id> gives
// the drill-down (detail, runs, rename, archive). Shared by every endpoint via
// tryHandleControlCommand — IM gets the same short lines.

const taskUsage = "Usage: /task <id> [runs|rename <name>|archive]"

// tasksOverviewReply renders /tasks and its variants: "" (open work),
// "done" (terminal, non-archived), "archived", "all".
func (d *Server) tasksOverviewReply(ctx context.Context, identity *control.IdentityContext, variant string) (string, error) {
	tasks, err := d.Control.ListTasks(ctx, identity.TenantID, identity.PersonID, 100)
	if err != nil {
		return "", err
	}
	runCounts, _ := d.Control.RunCountsByPerson(ctx, identity.TenantID, identity.PersonID)
	wsNames := d.workspaceNames(ctx, identity)

	var open, done, archived []control.Task
	for _, t := range tasks {
		switch {
		case archivedTaskStatus(t.Status):
			archived = append(archived, t)
		case terminalTaskStatus(t.Status):
			done = append(done, t)
		default:
			open = append(open, t)
		}
	}

	switch strings.TrimSpace(variant) {
	case "":
		activeTaskID := ""
		if active := d.coordinator().currentActive(identity.PersonID); active != nil {
			activeTaskID = active.TaskID
		}
		approvalCounts := d.pendingApprovalCounts(ctx, identity)
		var sb strings.Builder
		if len(open) == 0 {
			sb.WriteString("No open tasks.")
		} else {
			sb.WriteString("Open tasks:\n")
			for i, t := range open {
				sb.WriteString(fmt.Sprintf("%d. %s — %s\n", i+1,
					taskOverviewLine(t, runCounts[t.ID], wsNames[t.WorkspaceID], activeTaskID),
					taskNextStepHint(t, activeTaskID, approvalCounts[t.ID])))
			}
		}
		if n := len(done); n > 0 {
			sb.WriteString(fmt.Sprintf("\n… and %d done — /tasks done", n))
		}
		if n := len(archived); n > 0 {
			sb.WriteString(fmt.Sprintf("\n%d archived — /tasks archived", n))
		}
		return strings.TrimSpace(sb.String()), nil
	case "done":
		return renderTaskList("Done tasks", done, runCounts, wsNames), nil
	case "archived":
		return renderTaskList("Archived tasks", archived, runCounts, wsNames), nil
	case "all":
		return renderTaskList("All tasks", tasks, runCounts, wsNames), nil
	default:
		return "Usage: /tasks [done|archived|all]", nil
	}
}

// taskOverviewLine is one label's aggregate line:
// `[status] title (task_xxxxxxxx) · run: N 次 · <age> · <workspace>`.
func taskOverviewLine(t control.Task, runs int, wsName, activeTaskID string) string {
	label := t.Status
	switch {
	case t.ID == activeTaskID:
		label = "running"
	case isParkedTaskStatus(t.Status):
		label = t.Status + " · paused"
	}
	line := fmt.Sprintf("[%s] %s (%s) · run: %d 次 · %s",
		label, textutil.Truncate(toOneLine(t.Title), 48), shortTaskID(t.ID), runs, humanAge(t.UpdatedAt))
	if wsName != "" {
		line += " · " + wsName
	}
	return line
}

// taskNextStepHint tells the person what unblocks or continues each open
// label, so /tasks reads as a worklist rather than a status dump.
func taskNextStepHint(t control.Task, activeTaskID string, pendingApprovals int) string {
	switch {
	case t.ID == activeTaskID:
		return "running now (/status)"
	case pendingApprovals > 0:
		return fmt.Sprintf("waiting approval (%d pending — /approvals)", pendingApprovals)
	case strings.EqualFold(t.Status, "blocked"):
		return "waiting on you (/status)"
	default:
		return "reply to continue or /resume " + shortTaskID(t.ID)
	}
}

func renderTaskList(header string, tasks []control.Task, runCounts map[string]int, wsNames map[string]string) string {
	if len(tasks) == 0 {
		return "No " + strings.ToLower(header) + "."
	}
	var sb strings.Builder
	sb.WriteString(header + ":\n")
	for i, t := range tasks {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, taskOverviewLine(t, runCounts[t.ID], wsNames[t.WorkspaceID], "")))
	}
	return strings.TrimSpace(sb.String())
}

// taskCommandReply handles /task <id> [runs|rename <name>|archive].
func (d *Server) taskCommandReply(ctx context.Context, identity *control.IdentityContext, args []string) (string, error) {
	if len(args) == 0 {
		return taskUsage, nil
	}
	task, err := d.findTaskByRef(ctx, identity, args[0])
	if err != nil {
		return "", err
	}
	if task == nil {
		return "Task not found. Run /tasks to list ids.", nil
	}
	sub := ""
	if len(args) > 1 {
		sub = strings.ToLower(args[1])
	}
	switch sub {
	case "":
		return d.taskDetailReply(ctx, identity, task)
	case "runs":
		runs, err := d.Control.ListTaskRuns(ctx, identity.TenantID, task.ID, 10)
		if err != nil {
			return "", err
		}
		if len(runs) == 0 {
			return fmt.Sprintf("Task %s has no runs yet.", shortTaskID(task.ID)), nil
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "Runs for %s:\n", textutil.Truncate(toOneLine(task.Title), 48))
		for i, r := range runs {
			line := fmt.Sprintf("%d. [%s] %s", i+1, r.Status, humanAge(r.StartedAt))
			if s := strings.TrimSpace(r.InputSummary); s != "" {
				line += " · " + textutil.Truncate(toOneLine(s), 80)
			}
			sb.WriteString(line + "\n")
		}
		return strings.TrimSpace(sb.String()), nil
	case "rename":
		name := strings.TrimSpace(strings.Join(args[2:], " "))
		if name == "" {
			return "Usage: /task <id> rename <new name>", nil
		}
		name = textutil.Truncate(toOneLine(name), 80)
		if err := d.Control.RenameTask(ctx, identity.TenantID, task.ID, name); err != nil {
			return "", err
		}
		return fmt.Sprintf("Renamed task %s to: %s", shortTaskID(task.ID), name), nil
	case "archive":
		if archivedTaskStatus(task.Status) {
			return "Task is already archived.", nil
		}
		// Archived is terminal for continuation and hidden from open lists,
		// label cards, and the pre-label guess; only an explicit /resume <id>
		// reopens it. Keep the existing summary (empty summary is preserved by
		// UpdateTaskStatus's COALESCE).
		if err := d.Control.UpdateTaskStatus(ctx, identity.TenantID, task.ID, "archived", "", nil); err != nil {
			return "", err
		}
		_, _ = d.Control.AppendEvent(ctx, control.Event{
			TaskID:     task.ID,
			Type:       "task.archived",
			Visibility: "task",
			Payload:    mustJSON(map[string]string{"reason": "archived by user"}),
		})
		return fmt.Sprintf("Archived task: %s (%s). /resume %s reopens it.",
			textutil.Truncate(toOneLine(task.Title), 48), shortTaskID(task.ID), shortTaskID(task.ID)), nil
	default:
		return taskUsage, nil
	}
}

// taskDetailReply renders the /task <id> drill-down card.
func (d *Server) taskDetailReply(ctx context.Context, identity *control.IdentityContext, task *control.Task) (string, error) {
	handoff, _ := d.Control.LatestHandoff(ctx, task.ID)
	runs, _ := d.Control.ListTaskRuns(ctx, identity.TenantID, task.ID, 50)
	var sb strings.Builder
	fmt.Fprintf(&sb, "Task: %s\n", task.Title)
	fmt.Fprintf(&sb, "ID: %s\n", task.ID)
	statusText := task.Status
	active := d.coordinator().currentActive(identity.PersonID)
	if (active == nil || active.TaskID != task.ID) && isParkedTaskStatus(task.Status) {
		statusText += " (turn finished — reply to continue, or /resume " + shortTaskID(task.ID) + ")"
	}
	fmt.Fprintf(&sb, "Status: %s\n", statusText)
	if task.WorkspaceID != "" {
		name := d.workspaceNames(ctx, identity)[task.WorkspaceID]
		if name == "" {
			name = task.WorkspaceID
		}
		fmt.Fprintf(&sb, "Workspace: %s\n", name)
	}
	fmt.Fprintf(&sb, "Runs: %d\n", len(runs))
	fmt.Fprintf(&sb, "Updated: %s\n", humanAge(task.UpdatedAt))
	if s := strings.TrimSpace(task.CurrentSummary); s != "" {
		fmt.Fprintf(&sb, "Summary: %s\n", textutil.Truncate(toOneLine(s), 240))
	}
	nextSteps := task.NextSteps
	if len(nextSteps) == 0 && handoff != nil {
		nextSteps = handoff.NextSteps
	}
	if len(nextSteps) > 0 {
		sb.WriteString("Next:\n")
		for _, step := range nextSteps {
			fmt.Fprintf(&sb, "- %s\n", textutil.Truncate(toOneLine(step), 160))
		}
	}
	if handoff != nil && len(handoff.ChangedFiles) > 0 {
		sb.WriteString("Files:\n")
		for _, f := range handoff.ChangedFiles {
			fmt.Fprintf(&sb, "- %s\n", f)
		}
	}
	sb.WriteString(taskUsage)
	return strings.TrimSpace(sb.String()), nil
}

// findTaskByRef resolves a task reference for control commands: the full id,
// or a unique prefix (the short `task_xxxxxxxx` form the aggregated view
// prints). Only the person's own tasks resolve.
func (d *Server) findTaskByRef(ctx context.Context, identity *control.IdentityContext, ref string) (*control.Task, error) {
	ref = strings.TrimSpace(ref)
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

// pendingApprovalCounts groups the person's pending approvals by task for the
// /tasks next-step hints.
func (d *Server) pendingApprovalCounts(ctx context.Context, identity *control.IdentityContext) map[string]int {
	out := map[string]int{}
	pending, err := d.Control.ListApprovalRequests(ctx, identity.TenantID, identity.PersonID, "pending", 100)
	if err != nil {
		return out
	}
	for _, ap := range pending {
		if ap.TaskID != "" {
			out[ap.TaskID]++
		}
	}
	return out
}

// workspaceNames maps workspace id → display name for the person's
// workspaces. Best-effort: an error yields an empty map (lines drop the
// workspace column).
func (d *Server) workspaceNames(ctx context.Context, identity *control.IdentityContext) map[string]string {
	out := map[string]string{}
	workspaces, err := d.Control.ListWorkspaces(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		return out
	}
	for _, ws := range workspaces {
		out[ws.ID] = ws.Name
	}
	return out
}

// shortTaskID renders the compact display form `task_xxxxxxxx` used by the
// aggregated view; findTaskByRef resolves it back.
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
