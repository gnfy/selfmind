package httpapi

import (
	"context"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/platform/log"
)

func (d *Server) classifyIntent(ctx context.Context, identity *control.IdentityContext, input, channel string) router.IntentResult {
	if d == nil || d.Gateway == nil {
		return router.NewIntentClassifier().ClassifyDetailed(input)
	}
	// Implicit-continuation upgrade (Fix 1): the rules classifier defaults
	// ordinary language to IntentTask and shouldConsultIntentLLM hard-refuses to
	// consult the LLM for it, so an implicit follow-up ("质量太差了") after a just-
	// finished task would always create a NEW task with no context. When a recent
	// resumable task exists within the window, let the LLM decide continuation vs
	// new work. It may only upgrade task -> continue (never downgrade), so the
	// agent-first red line stays; at most one cheap LLM call per message, and only
	// in hybrid/llm mode when the window matches.
	if d.ContinueWindow > 0 && identity != nil {
		if rules := d.Gateway.ClassifyIntent(input); rules.Intent == router.IntentTask {
			if recent := d.recentResumableTask(ctx, identity); recent != nil {
				upgraded, ok := d.Gateway.UpgradeTaskToContinueWithLLM(ctx, input, channel, router.RecentTaskContext{
					Title:   recent.Title,
					Summary: recent.CurrentSummary,
					Status:  recent.Status,
					Age:     time.Since(recent.UpdatedAt),
				})
				if ok {
					log.Info("gateway: implicit continuation upgrade",
						"task", recent.ID, "confidence", upgraded.Confidence, "channel", channel)
					return upgraded
				}
			}
		}
	}
	return d.Gateway.ClassifyIntentWithContext(ctx, input, channel)
}

// recentResumableTask returns the person's most recently updated non-terminal
// task whose updated_at is within ContinueWindow, or nil. It is a cheap bounded
// scan (ListTasks is updated_at DESC, capped at 10) used only to decide whether
// the implicit-continuation LLM upgrade is worth a call — never to attach work
// on its own (attach still needs the IntentContinue verdict).
func (d *Server) recentResumableTask(ctx context.Context, identity *control.IdentityContext) *control.Task {
	if d == nil || d.Control == nil || identity == nil || d.ContinueWindow <= 0 {
		return nil
	}
	tasks, err := d.Control.ListTasks(ctx, identity.TenantID, identity.PersonID, 10)
	if err != nil {
		return nil
	}
	cutoff := time.Now().Add(-d.ContinueWindow)
	for i := range tasks {
		if terminalTaskStatus(tasks[i].Status) {
			continue
		}
		if tasks[i].UpdatedAt.Before(cutoff) {
			// DESC order: this non-terminal task and every later one are out of
			// the window, so there is nothing recent enough to continue.
			return nil
		}
		return &tasks[i]
	}
	return nil
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
// work later — absence of continuation evidence always means a new task.
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
	current, err := d.Control.CurrentTask(ctx, identity.TenantID, identity.PersonID)
	if err != nil || current != nil {
		return current, err
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
	if len(tasks) == 1 {
		return &tasks[0], nil
	}
	return nil, nil
}

func terminalTaskStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "done", "completed", "cancelled", "failed":
		return true
	default:
		return false
	}
}

func looksLikeAffirmativeContinuation(input string) bool {
	clean := strings.ToLower(strings.TrimSpace(input))
	clean = strings.Trim(clean, " \t\r\n.!?,;:。！？；：，")
	switch clean {
	case "ok", "okay", "yes", "y", "sure", "go ahead", "proceed", "sounds good",
		"\u53ef\u4ee5", "\u597d", "\u597d\u7684", "\u884c", "\u6ca1\u95ee\u9898",
		"\u540c\u610f", "\u5f00\u59cb", "\u5f00\u59cb\u5427", "\u6309\u8fd9\u4e2a\u505a",
		"\u5c31\u8fd9\u6837", "\u90a3\u5c31\u8fd9\u6837":
		return true
	default:
		return false
	}
}

func (c *RunCoordinator) withResumeContext(ctx context.Context, identity *control.IdentityContext, task *control.Task, run *control.Run, intent router.IntentResult, input string) string {
	if c == nil || c.srv == nil || c.srv.Control == nil || task == nil || intent.Intent != router.IntentContinue {
		return input
	}
	store := c.srv.Control
	handoff, _ := store.LatestHandoff(ctx, task.ID)
	events, _ := store.ListTaskEvents(ctx, task.ID, 8)
	if handoff == nil && len(events) == 0 {
		return input
	}
	runID := ""
	if run != nil {
		runID = run.ID
	}
	_, _ = store.AppendEvent(ctx, control.Event{
		TaskID:     task.ID,
		RunID:      runID,
		Type:       "run.resumed",
		Visibility: "task",
		Channel:    task.LastChannel,
		Payload: mustJSON(map[string]interface{}{
			"reason":     intent.Reason,
			"confidence": intent.Confidence,
		}),
	})

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
		writeResumeList(&sb, "changed_files", handoff.ChangedFiles)
		if handoff.TestStatus != "" {
			fmt.Fprintf(&sb, "test_status: %s\n", oneLine(handoff.TestStatus))
		}
		writeResumeList(&sb, "risks", handoff.Risks)
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
