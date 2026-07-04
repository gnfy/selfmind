package httpapi

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/platform/textutil"
)

// Control-command and status response formatters, extracted from server.go to
// keep that file focused on routing and request orchestration (see AGENTS.md).

func formatTasks(tasks []control.Task) string {
	if len(tasks) == 0 {
		return "No tasks."
	}
	var sb strings.Builder
	for i, task := range tasks {
		fmt.Fprintf(&sb, "%d. [%s] %s (%s)\n", i+1, task.Status, task.Title, task.ID)
	}
	return strings.TrimSpace(sb.String())
}

func formatQueue(items []control.QueuedTask) string {
	if len(items) == 0 {
		return "No queued tasks."
	}
	var sb strings.Builder
	sb.WriteString("Queued tasks:\n")
	for i, q := range items {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, textutil.Truncate(toOneLine(q.Content), 60))
	}
	sb.WriteString("\nUse /queue clear to drop all queued tasks.")
	return strings.TrimSpace(sb.String())
}

func formatWorkspaces(workspaces []control.Workspace) string {
	if len(workspaces) == 0 {
		return "No workspaces."
	}
	var sb strings.Builder
	for i, ws := range workspaces {
		fmt.Fprintf(&sb, "%d. %s (%s)\n   %s\n", i+1, ws.Name, ws.ID, ws.LocalPath)
	}
	return strings.TrimSpace(sb.String())
}

// approvalPayload is the persisted shape of a tool_call approval row's
// payload_json (written by toolApprovalHandler). Fields may be empty for
// non-tool approvals.
type approvalPayload struct {
	Tool   string                 `json:"tool"`
	Reason string                 `json:"reason"`
	Args   map[string]interface{} `json:"args"`
}

func decodeApprovalPayload(approval control.ApprovalRequest) approvalPayload {
	var p approvalPayload
	if len(approval.Payload) > 0 {
		_ = json.Unmarshal(approval.Payload, &p)
	}
	return p
}

// approvalArgsPreview renders the approval's tool args as a compact, bounded,
// UTF-8-safe "key=value" line so the user can see WHAT they are approving.
func approvalArgsPreview(args map[string]interface{}, maxChars int) string {
	if len(args) == 0 {
		return ""
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, fmt.Sprintf("%s=%v", k, args[k]))
	}
	return textutil.Truncate(toOneLine(strings.Join(pairs, " ")), maxChars)
}

// approvalSummaryLine is the one-line rich description of a pending approval,
// shared by the /approvals list and outbound approval notifications:
// `[tool] <args preview> — <reason> (task: <title>)`.
func approvalSummaryLine(approval control.ApprovalRequest, taskTitle string) string {
	p := decodeApprovalPayload(approval)
	label := fallback(p.Tool, approval.ActionType)
	var sb strings.Builder
	fmt.Fprintf(&sb, "[%s]", label)
	if preview := approvalArgsPreview(p.Args, 80); preview != "" {
		sb.WriteString(" " + preview)
	}
	if reason := strings.TrimSpace(p.Reason); reason != "" {
		sb.WriteString(" — " + textutil.Truncate(toOneLine(reason), 120))
	}
	if title := strings.TrimSpace(taskTitle); title != "" {
		fmt.Fprintf(&sb, " (task: %s)", textutil.Truncate(toOneLine(title), 30))
	}
	return sb.String()
}

func formatApprovals(approvals []control.ApprovalRequest, taskTitles map[string]string) string {
	if len(approvals) == 0 {
		return "No pending approvals."
	}
	var sb strings.Builder
	sb.WriteString("Pending approvals:\n")
	for i, approval := range approvals {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, approvalSummaryLine(approval, taskTitles[approval.TaskID]))
		fmt.Fprintf(&sb, "   %s\n", approval.ID)
	}
	sb.WriteString("\nUse /approve <number> or /reject <number>.")
	return strings.TrimSpace(sb.String())
}

func formatEvents(events []control.Event) string {
	if len(events) == 0 {
		return "No recent task events."
	}
	var sb strings.Builder
	sb.WriteString("Recent events:\n")
	for i, event := range events {
		fmt.Fprintf(&sb, "%d. %s", i+1, event.Type)
		if event.Channel != "" {
			fmt.Fprintf(&sb, " [%s]", event.Channel)
		}
		if len(event.Payload) > 0 && string(event.Payload) != "{}" {
			fmt.Fprintf(&sb, " %s", truncate(toOneLine(string(event.Payload)), 160))
		}
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String())
}

func toOneLine(value string) string {
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\r", " ")
	for strings.Contains(value, "  ") {
		value = strings.ReplaceAll(value, "  ", " ")
	}
	return strings.TrimSpace(value)
}

func formatIdentity(identity *control.IdentityContext) string {
	if identity == nil {
		return "No identity."
	}
	return fmt.Sprintf("tenant_id: %s\nperson_id: %s\naccount_id: %s\nplatform: %s\nplatform_user_id: %s",
		identity.TenantID, identity.PersonID, identity.AccountID, identity.Platform, identity.PlatformUserID)
}

func formatBusyRun(active *activeRun) string {
	if active == nil {
		return ""
	}
	elapsed := time.Since(active.StartedAt).Round(time.Second)
	// Conversational surface: no task/run hashes (ids live in the control
	// plane; /tasks and the HTTP API expose them when actually needed).
	title := strings.TrimSpace(active.Summary)
	if title == "" {
		title = "(starting)"
	}
	return fmt.Sprintf("A task is already running: %s\n- elapsed: %s\n\nUse /status for details or /stop to cancel.", textutil.Truncate(toOneLine(title), 60), elapsed)
}

func formatActiveRunStatus(active *activeRun) *api.ActiveRunStatus {
	if active == nil {
		return nil
	}
	return &api.ActiveRunStatus{
		TenantID:       active.TenantID,
		PersonID:       active.PersonID,
		TaskID:         active.TaskID,
		RunID:          active.RunID,
		Channel:        active.Channel,
		Summary:        active.Summary,
		StartedAt:      active.StartedAt.Format(time.RFC3339),
		ElapsedSeconds: int64(time.Since(active.StartedAt).Seconds()),
	}
}

func formatTaskStatus(task *control.Task, handoff *control.Handoff, active *activeRun, plan []taskPlanStep) string {
	if task == nil {
		return "No active task."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Task: %s\nStatus: %s\n", task.Title, task.Status)
	if active != nil {
		fmt.Fprintf(&sb, "\nRunning: %s elapsed\n", time.Since(active.StartedAt).Round(time.Second))
	}
	if task.CurrentSummary != "" {
		fmt.Fprintf(&sb, "\nSummary: %s\n", task.CurrentSummary)
	}
	if len(plan) > 0 {
		sb.WriteString("\nPlan:\n")
		for _, step := range plan {
			marker := "[ ]"
			switch step.Status {
			case "completed":
				marker = "[x]"
			case "in_progress":
				marker = "[>]"
			case "cancelled":
				marker = "[-]"
			}
			fmt.Fprintf(&sb, "- %s %s\n", marker, step.Step)
		}
	}
	if handoff != nil {
		if len(handoff.DoneItems) > 0 {
			sb.WriteString("\nDone:\n")
			for _, item := range handoff.DoneItems {
				fmt.Fprintf(&sb, "- %s\n", item)
			}
		}
		if handoff.TestStatus != "" {
			fmt.Fprintf(&sb, "\nTests:\n%s\n", handoff.TestStatus)
		}
		if len(handoff.ChangedFiles) > 0 {
			sb.WriteString("\nFiles:\n")
			for _, file := range handoff.ChangedFiles {
				fmt.Fprintf(&sb, "- %s\n", file)
			}
		}
	}
	nextSteps := task.NextSteps
	if len(nextSteps) == 0 && handoff != nil {
		nextSteps = handoff.NextSteps
	}
	if len(nextSteps) > 0 {
		sb.WriteString("\nNext:\n")
		for _, step := range nextSteps {
			fmt.Fprintf(&sb, "- %s\n", step)
		}
	}
	if handoff != nil && len(handoff.Risks) > 0 {
		sb.WriteString("\nRisks:\n")
		for _, risk := range handoff.Risks {
			fmt.Fprintf(&sb, "- %s\n", risk)
		}
	}
	return strings.TrimSpace(sb.String())
}
