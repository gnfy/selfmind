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

// isParkedTaskStatus reports whether a status is the non-terminal, resumable
// between-turns state a task rests in once its run finished: in_progress (parked
// between turns) or interrupted (recovered after a crash/sweep). Such a task is
// waiting for the person to continue, not actively working — but only when no
// run is live, which the caller checks separately.
func isParkedTaskStatus(status string) bool {
	switch strings.TrimSpace(strings.ToLower(status)) {
	case "in_progress", "interrupted", api.RunStatusVerificationPartial:
		return true
	default:
		return false
	}
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

// formatWorkspaces renders the numbered /workspaces list. The caller must pass
// the listWorkspacesForDisplay order: the numbers printed here are what
// /workspace <n> resolves (resolveWorkspaceReference), so display order IS
// resolution order.
func formatWorkspaces(workspaces []control.Workspace, currentID string) string {
	if len(workspaces) == 0 {
		return "No workspaces."
	}
	var sb strings.Builder
	for i, ws := range workspaces {
		marker := ""
		if currentID != "" && ws.ID == currentID {
			marker = "   ← current"
		}
		trust := ""
		if ws.TrustLevel == "untrusted" {
			trust = " [untrusted]"
		}
		fmt.Fprintf(&sb, "%d. %s (%s)%s%s\n   %s\n", i+1, ws.Name, ws.ID, marker, trust, ws.LocalPath)
	}
	sb.WriteString("\nUse /workspace <number> (or the id) to switch.")
	return strings.TrimSpace(sb.String())
}

// formatWorkspacesForIM uses transport-neutral plain text. Some IM clients
// reinterpret "1." lines and indentation as a rich-text ordered list, which
// can detach a workspace name from its path. Keep every workspace on one
// logical line and use bracketed ordinals that survive wrapping unchanged.
func formatWorkspacesForIM(workspaces []control.Workspace, currentID string) string {
	if len(workspaces) == 0 {
		return "No WS configured."
	}
	var sb strings.Builder
	sb.WriteString("WS\n\n")
	for i, ws := range workspaces {
		marker := ""
		if currentID != "" && ws.ID == currentID {
			marker = " [current]"
		}
		trust := ""
		if ws.TrustLevel == "untrusted" {
			trust = " [untrusted]"
		}
		fmt.Fprintf(&sb, "[%d] %s%s%s | %s\n", i+1, ws.Name, marker, trust, ws.LocalPath)
	}
	sb.WriteString("\nSwitch: /ws 2")
	return strings.TrimSpace(sb.String())
}

func formatWorkspaceSwitchForIM(ws *control.Workspace) string {
	if ws == nil {
		return "WS not found."
	}
	return fmt.Sprintf("WS switched\n\n%s\n%s", ws.Name, ws.LocalPath)
}

// approvalPayload is the persisted shape of a tool_call approval row's
// payload_json (written by toolApprovalHandler). Fields may be empty for
// non-tool approvals.
type approvalPayload struct {
	Tool   string                 `json:"tool"`
	Target string                 `json:"target,omitempty"`
	Reason string                 `json:"reason"`
	Args   map[string]interface{} `json:"args"`
	// GrantClass describes what a "remember this" decision authorizes. Empty
	// means the class is not reusable, so no grant was or will be recorded.
	GrantClass    string `json:"grant_class,omitempty"`
	RunGrantClass string `json:"run_grant_class,omitempty"`
	Containment   string `json:"containment,omitempty"`
	// Environment, Cwd, and ChangeSummary are the decision context written by
	// toolApprovalHandler: where the operation would run and how large the write
	// is. Display-only; they never widen what the approval authorizes.
	Environment   string `json:"environment,omitempty"`
	Cwd           string `json:"cwd,omitempty"`
	ChangeSummary string `json:"change_summary,omitempty"`
	// TriageState is tools.TriageStateUnavailable when smart-mode triage could
	// not rule on this call (no judge, error, timeout), so the ask is a fail-safe
	// fallback rather than a considered escalation.
	TriageState string `json:"triage_state,omitempty"`
	// DecisionPolicy is authoritative server-issued answer policy. In
	// particular, mode changes must never auto-resolve a once-only sensitive ask.
	DecisionPolicy string `json:"decision_policy,omitempty"`
	// TriageRationale, TriageRisk, and TriageAuthorization are the judge's
	// structured assessment when triage ran and handed the call to a human. They
	// are shown at decision time and kept for audit.
	TriageRationale     string `json:"triage_rationale,omitempty"`
	TriageRisk          string `json:"triage_risk,omitempty"`
	TriageAuthorization string `json:"triage_authorization,omitempty"`
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
		state := ""
		if approval.WaiterState == "parked" {
			state = " [parked " + approvalAge(approval, time.Now()) + "; answering resumes the task]"
		} else {
			state = " [pending " + approvalAge(approval, time.Now()) + "]"
		}
		fmt.Fprintf(&sb, "%d. %s%s\n", i+1, approvalSummaryLine(approval, taskTitles[approval.TaskID]), state)
		fmt.Fprintf(&sb, "   %s\n", approval.ID)
	}
	sb.WriteString("\nUse /approve <number> or /reject <number>.")
	return strings.TrimSpace(sb.String())
}

func approvalAge(approval control.ApprovalRequest, now time.Time) string {
	started := approval.CreatedAt
	if approval.WaiterState == "parked" && approval.ParkedAt != nil {
		started = *approval.ParkedAt
	}
	age := now.Sub(started)
	if age < 0 {
		age = 0
	}
	if age < time.Minute {
		return "<1m"
	}
	if age < time.Hour {
		return fmt.Sprintf("%dm", int(age/time.Minute))
	}
	if age < 24*time.Hour {
		return fmt.Sprintf("%dh", int(age/time.Hour))
	}
	return fmt.Sprintf("%dd", int(age/(24*time.Hour)))
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

// formatSteeredIntoRun is the conversational acknowledgement that a
// continuation was injected into the running task (cross-endpoint steering). No
// task/run hashes — ids stay in the control plane.
func formatSteeredIntoRun(active *activeRun) string {
	if active == nil {
		return "Added your guidance to the running task."
	}
	title := strings.TrimSpace(active.Summary)
	if title == "" {
		title = "the running task"
	}
	elapsed := time.Since(active.StartedAt).Round(time.Second)
	if active.StartedAt.IsZero() || elapsed < 0 {
		elapsed = 0
	}
	return fmt.Sprintf("Added your guidance to %s.\n- status: running\n- elapsed: %s\n\nIt will pick this up at the next safe step.", textutil.Truncate(toOneLine(title), 60), elapsed)
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
	statusText := task.Status
	// A task parked in the between-turns resumable state (in_progress/interrupted)
	// with NO live run finished its turn — it is not stuck. Say so, or the user
	// reads "in_progress" as "still working" and waits on a run that already ended
	// (observed live: 13 minutes staring at in_progress after the turn completed).
	if active == nil && isParkedTaskStatus(task.Status) {
		if strings.EqualFold(strings.TrimSpace(task.Status), api.RunStatusVerificationPartial) {
			statusText = "verification_partial (work changed; verification incomplete - reply to continue, or /new)"
		} else {
			statusText = task.Status + " (turn finished — reply to continue, or /new)"
		}
	}
	fmt.Fprintf(&sb, "Task: %s\nStatus: %s\n", task.Title, statusText)
	if active != nil {
		fmt.Fprintf(&sb, "\nRunning: %s elapsed\n", time.Since(active.StartedAt).Round(time.Second))
	}
	if task.CurrentSummary != "" {
		fmt.Fprintf(&sb, "\nSummary: %s\n", task.CurrentSummary)
	}
	if len(plan) > 0 {
		sb.WriteString("\nPlan:\n")
		for _, step := range plan {
			fmt.Fprintf(&sb, "- %s %s\n", planStatusMarker(step.Status), step.Step)
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

// planStatusMarker keeps plain-text plan rendering unambiguous across CLI and
// IM clients. In particular, "x" is commonly read as a failure rather than a
// checked checkbox when Markdown is not rendered.
func planStatusMarker(status string) string {
	switch status {
	case "completed":
		return "✓"
	case "in_progress":
		return "→"
	case "cancelled":
		return "−"
	default:
		return "○"
	}
}
