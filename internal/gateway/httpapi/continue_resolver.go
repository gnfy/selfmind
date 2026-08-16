package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
)

// classifyIntent is rules-first intent classification (plus the router's own
// LLM consult for ambiguous non-task input). The implicit-continuation LLM
// upgrade that used to live here was REMOVED with Work Timeline P3: context is
// spine-based (P1) so task attachment no longer affects what the model sees —
// a pre-agent "does this continue that task?" call bought nothing. Explicit
// rules-based IntentContinue (cues, short acceptance) remains: it still drives
// the busy/steer path and deterministic continue semantics.
func (d *Server) classifyIntent(ctx context.Context, input, channel string) router.IntentResult {
	if d == nil || d.Gateway == nil {
		return router.NewIntentClassifier().ClassifyDetailed(input)
	}
	return d.Gateway.ClassifyIntentWithContext(ctx, input, channel)
}

func (d *Server) tryHandleIntentClarification(identity *control.IdentityContext, intent router.IntentResult) (bool, api.MessageResponse) {
	return false, api.MessageResponse{}
}

func (d *Server) tryHandleDirectIntent(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, intent router.IntentResult) (bool, api.MessageResponse) {
	return false, api.MessageResponse{}
}

func aggregateDirectResponse(resp *router.HandleResponse) (string, llm.UsageStats, error) {
	if resp == nil {
		return "", llm.UsageStats{}, nil
	}
	return router.AggregateFinalResponse(resp)
}

// resumePinKey is the person_settings key holding the one-shot "attach the
// next agent-bound message to this task" marker written by /resume. It is
// person-scoped (like resolveContinueTask) and consumed by the first message
// that reaches resolveTask, so a stale /resume can never capture unrelated new
// work later.
const resumePinKey = "resume_pin_task"

// consumeResumePin returns the task pinned by an explicit /resume and clears
// the pin in the same step (one-shot). A missing, foreign, or unreadable task
// yields nil so the caller falls through to new-task creation.
func (d *Server) consumeResumePin(ctx context.Context, identity *control.IdentityContext) *control.Task {
	if d == nil || d.Control == nil || identity == nil {
		return nil
	}
	taskID, err := d.Control.GetPersonSetting(ctx, identity.TenantID, identity.PersonID, resumePinKey)
	if err != nil || strings.TrimSpace(taskID) == "" {
		return nil
	}
	_ = d.Control.SetPersonSetting(ctx, identity.TenantID, identity.PersonID, resumePinKey, "")
	task, err := d.Control.GetTask(ctx, identity.TenantID, taskID)
	if err != nil || task == nil || task.PersonID != identity.PersonID {
		return nil
	}
	return task
}

func (d *Server) resolveContinueTask(ctx context.Context, identity *control.IdentityContext) (*control.Task, error) {
	if d == nil || d.Control == nil || identity == nil {
		return nil, nil
	}
	// An archived label is deliberately shelved: `继续` must never resurrect it
	// implicitly (only an explicit /resume <id> can — see resolveTask's pin).
	current, err := d.Control.CurrentTask(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		return nil, err
	}
	if current != nil && current.IsVisible() && !current.IsInbox() && !archivedTaskStatus(current.Status) {
		return current, nil
	}
	tasks, err := d.Control.ListTasks(ctx, identity.TenantID, identity.PersonID, 10)
	if err != nil {
		return nil, err
	}
	for _, task := range tasks {
		if !terminalTaskStatus(task.Status) {
			return &task, nil
		}
	}
	if len(tasks) == 1 && !archivedTaskStatus(tasks[0].Status) {
		return &tasks[0], nil
	}
	return nil, nil
}

// terminalTaskStatus reports whether a task status ends its life for implicit
// continuation and the pre-label default. `archived` (the /task <id> archive
// verb, Work Timeline P3) is terminal here: an archived label is excluded from
// open lists, recall label cards, and the pre-label guess — but an explicit
// /resume <id> may still reopen it deliberately (see resolveTask).
func terminalTaskStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "done", "completed", "cancelled", "failed", "archived":
		return true
	default:
		return false
	}
}

// archivedTaskStatus isolates the one terminal status a person may explicitly
// resurrect via /resume (a deliberate act, unlike done/cancelled/failed which
// stay closed).
func archivedTaskStatus(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "archived")
}

func looksLikeAffirmativeContinuation(input string) bool {
	clean := strings.ToLower(strings.TrimSpace(input))
	clean = strings.Trim(clean, " \t\r\n.!?,;:。！？；：，")
	switch clean {
	case "ok", "okay", "yes", "y", "sure", "go ahead", "proceed", "sounds good",
		"\u53ef\u4ee5", "\u597d", "\u597d\u7684", "\u884c", "\u6ca1\u95ee\u9898",
		"\u540c\u610f", "\u5f00\u59cb", "\u5f00\u59cb\u5427", "\u6309\u8fd9\u4e2a\u505a",
		"\u5f00\u59cb\u6267\u884c", "\u6267\u884c\u5427", "\u8bf7\u6267\u884c",
		"\u5c31\u8fd9\u6837", "\u90a3\u5c31\u8fd9\u6837":
		return true
	default:
		return false
	}
}

func (c *RunCoordinator) withResumeContext(ctx context.Context, identity *control.IdentityContext, task *control.Task, run *control.Run, intent router.IntentResult, explicitResume bool, workKey, input string) string {
	if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil || task == nil || (!explicitResume && intent.Intent != router.IntentContinue) {
		return input
	}
	store := c.srv.Control
	runID := ""
	resumedRuns := int64(0)
	if run != nil {
		runID = run.ID
		resumedRuns, _ = store.MarkTaskRunsResumed(ctx, identity.TenantID, task.ID, run.ID)
	}
	handoff, _ := store.LatestHandoff(ctx, task.ID)
	events, _ := store.ListTaskEvents(ctx, task.ID, 8)
	_, _ = store.AppendEvent(ctx, control.Event{
		TaskID:     task.ID,
		RunID:      runID,
		Type:       "run.resumed",
		Visibility: "task",
		Channel:    task.LastChannel,
		Payload: mustJSON(map[string]interface{}{
			"reason":            intent.Reason,
			"confidence":        intent.Confidence,
			"resumes_task_runs": resumedRuns > 0,
			"resumed_run_count": resumedRuns,
			"work_key":          strings.ToUpper(strings.TrimSpace(workKey)),
		}),
	})
	if handoff == nil && len(events) == 0 {
		return input
	}
	if kernel.HasLoopResumeMessages(ctx) {
		return input
	}

	var sb strings.Builder
	sb.WriteString("[SelfMind resume context]\n")
	fmt.Fprintf(&sb, "task_id: %s\n", task.ID)
	fmt.Fprintf(&sb, "task_title: %s\n", task.Title)
	fmt.Fprintf(&sb, "task_status: %s\n", task.Status)
	if task.CurrentSummary != "" {
		fmt.Fprintf(&sb, "current_summary: %s\n", task.CurrentSummary)
	}
	if handoff != nil {
		if handoff.Summary != "" {
			fmt.Fprintf(&sb, "handoff_summary: %s\n", handoff.Summary)
		}
		writeResumeList(&sb, "done", handoff.DoneItems)
		writeResumeList(&sb, "next_steps", handoff.NextSteps)
		if handoff.TestStatus != "" {
			fmt.Fprintf(&sb, "test_status: %s\n", oneLine(handoff.TestStatus))
		}
		writeResumeList(&sb, "risks", handoff.Risks)
	}
	// The files this task already created/edited — merged from the handoff and
	// the task's file-mutating tool events (write_file/patch). An interrupted run
	// (e.g. provider EOF) leaves no handoff, so the events are the only record of
	// what was built; surfacing the exact paths stops the continuation from
	// re-listing the directory and editing the wrong file (observed live: a
	// resumed game-build task rediscovered and overwrote an unrelated .html).
	if files := c.resumeChangedFiles(ctx, task, handoff, 10); len(files) > 0 {
		sb.WriteString("files_this_task_created_or_changed:\n")
		for _, f := range files {
			fmt.Fprintf(&sb, "- %s\n", f)
		}
		sb.WriteString("Edit these existing files directly to continue; do not re-search the workspace or recreate them unless the user asks for a fresh start.\n")
	}
	// Spooled large tool outputs from earlier runs of this task (W1): the
	// continuation can read any byte range by reference instead of re-running
	// the commands that produced them.
	if artifacts, err := store.ListTaskArtifacts(ctx, task.ID, 20); err == nil {
		listed := 0
		for _, artifact := range artifacts {
			if artifact.Kind != "tool_output" || listed >= 5 {
				continue
			}
			if listed == 0 {
				sb.WriteString("large_tool_outputs (read with tool_output_view, do not re-run the commands):\n")
			}
			var meta struct {
				Tool  string `json:"tool"`
				Bytes int    `json:"bytes"`
			}
			_ = json.Unmarshal(artifact.Metadata, &meta)
			tool := meta.Tool
			if tool == "" {
				tool = artifact.Name
			}
			fmt.Fprintf(&sb, "- artifact_id=%s tool=%s bytes=%d\n", artifact.ID, tool, meta.Bytes)
			listed++
		}
	}
	if len(events) > 0 {
		sb.WriteString("recent_events:\n")
		for _, event := range events {
			fmt.Fprintf(&sb, "- %s", event.Type)
			if event.Channel != "" {
				fmt.Fprintf(&sb, " channel=%s", event.Channel)
			}
			if len(event.Payload) > 0 && string(event.Payload) != "{}" {
				fmt.Fprintf(&sb, " payload=%s", truncate(oneLine(string(event.Payload)), 240))
			}
			sb.WriteString("\n")
		}
	}
	// Re-inject the live plan (with per-step status) so a resumed task continues
	// from the right step instead of losing its in-progress plan.
	if plan := c.srv.latestPlanForTask(ctx, task.ID); len(plan) > 0 {
		sb.WriteString("current_plan:\n")
		for _, step := range plan {
			status := strings.TrimSpace(step.Status)
			if status == "" {
				status = "pending"
			}
			fmt.Fprintf(&sb, "- [%s] %s\n", status, oneLine(step.Step))
		}
	}
	sb.WriteString("Continue from this state. Do not restart completed work unless the user asks for a restart.\n")
	sb.WriteString("[/SelfMind resume context]\n\n")
	sb.WriteString(input)
	return sb.String()
}

// resumeChangedFiles returns up to `limit` distinct file paths this task has
// already created or edited, most-authoritative first: the handoff's changed
// files (when a run finalized), then paths recovered from the task's
// file-mutating tool events (the only source when a run was interrupted before
// finalization). Bounded and derived — it never injects raw event rows into the
// prompt, staying inside the resume-context contract (docs/context-lifecycle).
// withUncertainToolWarning injects the task's uncertain side-effect ledger
// entries (dispatched, outcome never recorded — a prior crash) regardless of
// intent classification (P0-B closure). The safety property belongs to the
// TASK, not the continuation intent: a requeued 'started' row re-drains with
// its ORIGINAL content, which classifies as a new message, so gating this on
// IntentContinue would let a boot-requeued run silently re-fire a
// deploy/build/POST. Any run touching a task with uncertain side effects is
// told to verify real state read-only before repeating.
func (c *RunCoordinator) withUncertainToolWarning(ctx context.Context, identity *control.IdentityContext, task *control.Task, input string) string {
	if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil || task == nil {
		return input
	}
	entries, err := c.srv.Control.ListUncertainToolEntriesForTask(ctx, identity.TenantID, task.ID, 10)
	if err != nil || len(entries) == 0 {
		return input
	}
	var sb strings.Builder
	sb.WriteString("[SelfMind uncertain tool calls]\n")
	sb.WriteString("A prior run of this task dispatched these side-effectful tool calls but crashed before recording their outcome. They may or may not have taken effect. VERIFY the real external state with a read-only check before repeating any of them; do NOT blindly re-run (a second deploy/build/network POST could result):\n")
	for _, e := range entries {
		fmt.Fprintf(&sb, "- tool=%s class=%s dispatched=%s (outcome unrecorded)\n",
			e.ToolName, e.RetryClass, e.CreatedAt.Format("01-02 15:04"))
	}
	return strings.TrimSpace(sb.String()) + "\n\n" + input
}

func (c *RunCoordinator) resumeChangedFiles(ctx context.Context, task *control.Task, handoff *control.Handoff, limit int) []string {
	if c == nil || c.srv == nil || c.srv.Control == nil || task == nil || limit <= 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] || len(out) >= limit {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	if handoff != nil {
		for _, f := range handoff.ChangedFiles {
			add(f)
		}
	}
	// ListTaskEvents is newest-first; scan a bounded window for file-mutating
	// tool calls. tool.started args carry the exact target path (write_file) or
	// the V4A patch text (patch); tool.completed result headers are a backup.
	events, _ := c.srv.Control.ListTaskEvents(ctx, task.ID, 80)
	for _, ev := range events {
		if len(out) >= limit {
			break
		}
		payload := decodeEventPayload(ev.Payload)
		tool := strings.TrimSpace(asString(payload["tool"]))
		switch ev.Type {
		case "tool.started":
			switch tool {
			case "patch", "apply_patch":
				for _, p := range patchPathsFromArgsJSON(asString(payload["args"])) {
					add(p)
				}
			case "write_file", "edit", "edit_file":
				// Single-path file-mutating tools carry the target under
				// path/file_path/output_path — never read_file (a read, not a change).
				add(pathFromArgsJSON(asString(payload["args"])))
			}
		case "tool.completed":
			switch tool {
			case "write_file", "edit", "edit_file":
				add(pathFromWriteResultHeader(asString(payload["result"])))
			}
		}
	}
	return out
}

func decodeEventPayload(raw json.RawMessage) map[string]interface{} {
	if len(raw) == 0 {
		return map[string]interface{}{}
	}
	m := map[string]interface{}{}
	_ = json.Unmarshal(raw, &m)
	return m
}

func asString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// pathFromArgsJSON extracts the target path from a single-path file tool's
// recorded args JSON (a JSON string held in the event payload). Different tools
// name the field differently (write_file: path; some edit tools: file_path or
// output_path), so it checks each in priority order.
func pathFromArgsJSON(argsJSON string) string {
	argsJSON = strings.TrimSpace(argsJSON)
	if argsJSON == "" {
		return ""
	}
	var args struct {
		Path       string `json:"path"`
		FilePath   string `json:"file_path"`
		OutputPath string `json:"output_path"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ""
	}
	for _, p := range []string{args.Path, args.FilePath, args.OutputPath} {
		if strings.TrimSpace(p) != "" {
			return p
		}
	}
	return ""
}

// patchPathsFromArgsJSON extracts every file path a V4A patch touches from the
// patch tool's recorded args JSON ({"patch": "*** Begin Patch ..."}).
func patchPathsFromArgsJSON(argsJSON string) []string {
	argsJSON = strings.TrimSpace(argsJSON)
	if argsJSON == "" {
		return nil
	}
	var args struct {
		Patch string `json:"patch"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(args.Patch, "\n") {
		line = strings.TrimSpace(line)
		for _, prefix := range []string{"*** Update File:", "*** Add File:", "*** Delete File:", "*** Move File:"} {
			if strings.HasPrefix(line, prefix) {
				rest := strings.TrimSpace(strings.TrimPrefix(line, prefix))
				// "Move File: old -> new" — keep the destination path.
				if idx := strings.Index(rest, "->"); idx >= 0 {
					rest = strings.TrimSpace(rest[idx+2:])
				}
				if rest != "" {
					out = append(out, rest)
				}
			}
		}
	}
	return out
}

// pathFromWriteResultHeader recovers the path from a write_file result header
// ("Created <path> (+A -B)", "Edited <path> (+A -B)", "No change to <path>").
func pathFromWriteResultHeader(result string) string {
	line := strings.TrimSpace(firstLine(result))
	if line == "" {
		return ""
	}
	for _, prefix := range []string{"Created ", "Edited ", "No change to "} {
		if strings.HasPrefix(line, prefix) {
			rest := strings.TrimSpace(strings.TrimPrefix(line, prefix))
			// Drop the trailing " (+A -B)" stats suffix if present.
			if idx := strings.LastIndex(rest, " (+"); idx >= 0 {
				rest = strings.TrimSpace(rest[:idx])
			}
			return rest
		}
	}
	return ""
}

func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
}

func writeResumeList(sb *strings.Builder, label string, values []string) {
	if len(values) == 0 {
		return
	}
	sb.WriteString(label)
	sb.WriteString(":\n")
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			fmt.Fprintf(sb, "- %s\n", value)
		}
	}
}

func oneLine(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	for strings.Contains(value, "  ") {
		value = strings.ReplaceAll(value, "  ", " ")
	}
	return strings.TrimSpace(value)
}
