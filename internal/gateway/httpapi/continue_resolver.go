package httpapi

import (
	"context"
	"fmt"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel/llm"
)

func (d *Server) classifyIntent(ctx context.Context, input, channel string) router.IntentResult {
	if d != nil && d.Gateway != nil {
		return d.Gateway.ClassifyIntentWithContext(ctx, input, channel)
	}
	return router.NewIntentClassifier().ClassifyDetailed(input)
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
