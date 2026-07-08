package httpapi

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/platform/textutil"
)

// /tasks aggregated view + /task <id> subcommands (Work Timeline P3,
// docs/work-timeline.md "/tasks view"). Tasks are labels: the default view
// shows only OPEN work as multi-line cards (simplified status bracket, last
// input, primary artifact, pending approvals/questions, run count, short id),
// collapses finished work to a count, and /task <id> gives the drill-down
// (detail, runs, rename, archive). Shared by every endpoint via
// tryHandleControlCommand — IM gets the same short cards.

const taskUsage = "Usage: /task <n|id> [runs|rename <name>|archive]"

const tasksTrailingHint = "Reply to continue the current task, /resume <id> to switch, /task <id> for detail."

// taskCardView is the batched per-person data every card draws from, fetched
// once per /tasks render (grouped queries, never per-task round trips).
type taskCardView struct {
	activeTaskID string              // task with a LIVE run right now, else ""
	runCounts    map[string]int      // task_id -> run count
	lastRuns     map[string]string   // task_id -> latest run input summary
	files        map[string][]string // task_id -> latest handoff changed files
	approvals    map[string]int      // task_id -> pending approvals
	questions    map[string]int      // task_id -> pending clarify questions
}

// tasksOverviewReply renders /tasks and its variants: ""/"open" (open work),
// "done" (terminal, non-archived), "archived", "all". Any other suffix is a
// keyword search across all tasks, e.g. /tasks game. A variant may also carry a
// query: /tasks done report, /tasks archived tank, /tasks search pgsql.
func (d *Server) tasksOverviewReply(ctx context.Context, identity *control.IdentityContext, variant string) (string, error) {
	tasks, err := d.listTasksForDisplay(ctx, identity)
	if err != nil {
		return "", err
	}
	view := taskCardView{}
	if active := d.coordinator().currentActive(identity.PersonID); active != nil {
		view.activeTaskID = active.TaskID
	}
	// Best-effort card data: a failed grouped query drops that column, never
	// the whole view.
	view.runCounts, _ = d.Control.RunCountsByPerson(ctx, identity.TenantID, identity.PersonID)
	view.lastRuns, _ = d.Control.LatestRunSummaries(ctx, identity.TenantID, identity.PersonID)
	view.files, _ = d.Control.LatestHandoffFilesByPerson(ctx, identity.TenantID, identity.PersonID)
	view.approvals, view.questions, _ = d.Control.PendingCountsByTask(ctx, identity.TenantID, identity.PersonID)

	open, done, archived := splitTasksForDisplay(tasks)
	args := parseTasksArgs(variant)
	if args.search {
		return renderTaskSearchResults(args, open, done, archived, tasks, view), nil
	}

	switch args.view {
	case "", "open":
		var sb strings.Builder
		if len(open) == 0 {
			sb.WriteString("No open tasks.")
		} else {
			sb.WriteString("Open tasks:\n")
			for i, t := range open {
				sb.WriteString("\n" + renderTaskCard(i+1, t, view) + "\n")
			}
		}
		if n := len(done); n > 0 {
			sb.WriteString(fmt.Sprintf("\n… and %d done — /tasks done", n))
		}
		if n := len(archived); n > 0 {
			sb.WriteString(fmt.Sprintf("\n%d archived — /tasks archived", n))
		}
		if len(open) > 0 {
			sb.WriteString("\n\n" + tasksTrailingHint)
		}
		return strings.TrimSpace(sb.String()), nil
	case "done":
		return renderTaskCards("Done tasks", done, view), nil
	case "archived":
		return renderTaskCards("Archived tasks", archived, view), nil
	case "all":
		return renderTaskCards("All tasks", tasks, view), nil
	default:
		return "Usage: /tasks [open|done|archived|all|search <keyword>|<keyword>]", nil
	}
}

type tasksArgs struct {
	view   string
	query  string
	search bool
}

func parseTasksArgs(input string) tasksArgs {
	fields := strings.Fields(strings.TrimSpace(input))
	if len(fields) == 0 {
		return tasksArgs{}
	}
	first := strings.ToLower(fields[0])
	switch first {
	case "open", "done", "archived", "all":
		return tasksArgs{
			view:   first,
			query:  strings.TrimSpace(strings.Join(fields[1:], " ")),
			search: len(fields) > 1,
		}
	case "search", "find", "query":
		return tasksArgs{
			view:   "all",
			query:  strings.TrimSpace(strings.Join(fields[1:], " ")),
			search: true,
		}
	default:
		return tasksArgs{view: "all", query: strings.TrimSpace(strings.Join(fields, " ")), search: true}
	}
}

func renderTaskSearchResults(args tasksArgs, open, done, archived, all []control.Task, view taskCardView) string {
	query := strings.TrimSpace(args.query)
	if query == "" {
		return "Usage: /tasks search <keyword>"
	}
	scope := all
	scopeLabel := "all"
	switch args.view {
	case "open":
		scope = open
		scopeLabel = "open"
	case "done":
		scope = done
		scopeLabel = "done"
	case "archived":
		scope = archived
		scopeLabel = "archived"
	case "all", "":
		scope = all
	}
	matches := filterTasksByQuery(scope, query, view)
	if len(matches) == 0 {
		if scopeLabel == "all" {
			return fmt.Sprintf("No tasks match %q.", query)
		}
		return fmt.Sprintf("No %s tasks match %q.", scopeLabel, query)
	}
	var sb strings.Builder
	if scopeLabel == "all" {
		fmt.Fprintf(&sb, "Matching tasks for %q:\n", query)
	} else {
		fmt.Fprintf(&sb, "Matching %s tasks for %q:\n", scopeLabel, query)
	}
	for i, t := range matches {
		sb.WriteString("\n" + renderTaskCard(i+1, t, view) + "\n")
	}
	sb.WriteString("\nUse /resume <id> from a card to switch, or /task <id> for detail.")
	return strings.TrimSpace(sb.String())
}

func filterTasksByQuery(tasks []control.Task, query string, view taskCardView) []control.Task {
	terms := strings.Fields(strings.ToLower(strings.TrimSpace(query)))
	if len(terms) == 0 {
		return nil
	}
	var out []control.Task
	for _, t := range tasks {
		haystack := strings.ToLower(strings.Join(taskSearchFields(t, view), "\n"))
		matched := true
		for _, term := range terms {
			if !strings.Contains(haystack, term) {
				matched = false
				break
			}
		}
		if matched {
			out = append(out, t)
		}
	}
	return out
}

func taskSearchFields(t control.Task, view taskCardView) []string {
	fields := []string{
		t.ID,
		cardTaskID(t.ID),
		shortTaskID(t.ID),
		t.Title,
		t.Status,
		t.CurrentSummary,
		strings.Join(t.NextSteps, " "),
		view.lastRuns[t.ID],
	}
	for _, file := range view.files[t.ID] {
		fields = append(fields, file, fileBasename(file))
	}
	return fields
}

// taskCardStatus maps one label to the simplified card bracket:
// running (live run) > done/cancelled/failed/archived verbatim > waiting
// (pending approval/question, or blocked) > paused (open, nothing executing —
// in_progress/interrupted/new all read as paused).
func taskCardStatus(t control.Task, isActive bool, pendingApprovals, pendingQuestions int) string {
	switch {
	case isActive:
		return "running"
	case terminalTaskStatus(t.Status):
		return strings.ToLower(strings.TrimSpace(t.Status))
	case pendingApprovals > 0 || pendingQuestions > 0 || strings.EqualFold(strings.TrimSpace(t.Status), "blocked"):
		return "waiting"
	default:
		return "paused"
	}
}

// renderTaskCard renders one label as a multi-line card:
//
//  1. [running] 拳皇97风格对战游戏
//     last: 再多做几个角色 · 3m ago
//     file: arcade-fury-97.html
//     approvals: 1
//     runs: 6
//     id: task_65de41f2a...
//
// file/approvals/questions lines are omitted when empty/zero; an interrupted
// label shows "· interrupted" instead of the age.
func renderTaskCard(index int, t control.Task, v taskCardView) string {
	var sb strings.Builder
	isActive := t.ID == v.activeTaskID
	status := taskCardStatus(t, isActive, v.approvals[t.ID], v.questions[t.ID])
	fmt.Fprintf(&sb, "%d. [%s] %s\n", index, status, truncateRunes(toOneLine(t.Title), 50))

	last := strings.TrimSpace(v.lastRuns[t.ID])
	if last == "" {
		last = strings.TrimSpace(t.CurrentSummary)
	}
	suffix := humanAge(t.UpdatedAt)
	if !isActive && strings.EqualFold(strings.TrimSpace(t.Status), "interrupted") {
		suffix = "interrupted"
	}
	if last != "" {
		fmt.Fprintf(&sb, "   last: %s · %s\n", truncateRunes(toOneLine(last), 40), suffix)
	} else {
		fmt.Fprintf(&sb, "   last: %s\n", suffix)
	}

	if files := v.files[t.ID]; len(files) > 0 {
		if name := fileBasename(files[0]); name != "" {
			fmt.Fprintf(&sb, "   file: %s\n", name)
		}
	}
	if n := v.approvals[t.ID]; n > 0 {
		fmt.Fprintf(&sb, "   approvals: %d\n", n)
	}
	if n := v.questions[t.ID]; n > 0 {
		fmt.Fprintf(&sb, "   questions: %d\n", n)
	}
	fmt.Fprintf(&sb, "   runs: %d\n", v.runCounts[t.ID])
	fmt.Fprintf(&sb, "   id: %s", cardTaskID(t.ID))
	return sb.String()
}

// renderTaskCards renders the done|archived|all variants with the same card
// format as the default view (no trailing hint — those lists are archives, not
// the active worklist).
func renderTaskCards(header string, tasks []control.Task, view taskCardView) string {
	if len(tasks) == 0 {
		return "No " + strings.ToLower(header) + "."
	}
	var sb strings.Builder
	sb.WriteString(header + ":\n")
	for i, t := range tasks {
		sb.WriteString("\n" + renderTaskCard(i+1, t, view) + "\n")
	}
	return strings.TrimSpace(sb.String())
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

// fileBasename extracts a display basename from a changed-file path, handling
// both / and \ separators (workspace paths may be Windows-style).
func fileBasename(path string) string {
	path = strings.TrimSpace(path)
	path = strings.TrimRight(path, "/\\")
	if i := strings.LastIndexAny(path, "/\\"); i >= 0 {
		path = path[i+1:]
	}
	return strings.TrimSpace(path)
}

// taskCommandReply handles /task <id> [runs|rename <name>|archive].
func (d *Server) taskCommandReply(ctx context.Context, identity *control.IdentityContext, args []string) (string, error) {
	if len(args) == 0 {
		return taskUsage, nil
	}
	task, userErr, err := d.resolveTaskReference(ctx, identity, args[0])
	if err != nil {
		return "", err
	}
	if userErr != "" {
		return userErr, nil
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

// listTasksForDisplay fetches the person's tasks in the stable display order
// every task list renders in. The store already orders by updated_at DESC;
// the id tiebreak here makes ties deterministic so the numbered cards and
// ordinal resolution can never disagree within one snapshot.
func (d *Server) listTasksForDisplay(ctx context.Context, identity *control.IdentityContext) ([]control.Task, error) {
	tasks, err := d.Control.ListTasks(ctx, identity.TenantID, identity.PersonID, 100)
	if err != nil {
		return nil, err
	}
	sortTasksForDisplay(tasks)
	return tasks, nil
}

// sortTasksForDisplay orders tasks newest-first (updated_at DESC, id ASC as
// tiebreaker). This is the card order of the /tasks views and therefore the
// order ordinal references (/task 1, /resume 1) resolve against; both sides
// must use it or numbers would pick the wrong task (same display-order =
// resolution-order contract as sortApprovalsForDisplay).
func sortTasksForDisplay(tasks []control.Task) {
	sort.SliceStable(tasks, func(i, j int) bool {
		if !tasks[i].UpdatedAt.Equal(tasks[j].UpdatedAt) {
			return tasks[i].UpdatedAt.After(tasks[j].UpdatedAt)
		}
		return tasks[i].ID < tasks[j].ID
	})
}

// splitTasksForDisplay partitions display-ordered tasks into open / done /
// archived, preserving order. The open slice IS the numbered card list of the
// default /tasks view, so it is also the list task ordinals resolve against.
func splitTasksForDisplay(tasks []control.Task) (open, done, archived []control.Task) {
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
	return open, done, archived
}

// resolveTaskReference resolves a user-supplied task reference for control
// commands, mirroring the approval resolver contract (approval_resolver.go):
// a bare number is a LIST ORDINAL against the numbered cards of the default
// /tasks view (open tasks, display order = resolution order — both sides go
// through listTasksForDisplay/splitTasksForDisplay), and anything else is a
// full id or unique short prefix (findTaskByRef; the card-displayed
// task_xxxxxxxx form round-trips). Shared by /task and /resume so the same
// reference means the same task on every surface.
//
// The middle return is a user-facing sentence (safe to send verbatim on any
// channel) for reference mistakes; the error return is reserved for storage
// failures.
func (d *Server) resolveTaskReference(ctx context.Context, identity *control.IdentityContext, ref string) (*control.Task, string, error) {
	ref = strings.TrimSpace(ref)
	if ordinal, convErr := strconv.Atoi(ref); convErr == nil {
		tasks, err := d.listTasksForDisplay(ctx, identity)
		if err != nil {
			return nil, "", err
		}
		open, _, _ := splitTasksForDisplay(tasks)
		if len(open) == 0 {
			return nil, "No open tasks to number; see /tasks.", nil
		}
		if ordinal < 1 || ordinal > len(open) {
			return nil, fmt.Sprintf("No open task number %d; %d open (see /tasks).", ordinal, len(open)), nil
		}
		return &open[ordinal-1], "", nil
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

// shortTaskID renders the compact reference form `task_xxxxxxxx` used in
// hints (/resume task_xxxxxxxx); findTaskByRef resolves it back.
func shortTaskID(id string) string {
	const prefix = "task_"
	if strings.HasPrefix(id, prefix) && len(id) > len(prefix)+8 {
		return id[:len(prefix)+8]
	}
	return id
}

// cardTaskID renders the card's trailing id line: `task_` + the first 9 chars
// of the uuid part + `...` (a dangling `-` separator is dropped — for v4
// uuids char 9 is always the group hyphen). findTaskByRef strips the trailing
// dots, so a pasted card id resolves like any unique prefix.
func cardTaskID(id string) string {
	const prefix = "task_"
	if strings.HasPrefix(id, prefix) && len(id) > len(prefix)+9 {
		// Short prefix only, no ellipsis (owner preference): the prefix is a
		// valid /resume reference as-is, and the dots read as visual noise.
		return strings.TrimRight(id[:len(prefix)+9], "-")
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
