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
	if identity == nil || !intent.NeedsClarification {
		return false, api.MessageResponse{}
	}
	question := strings.TrimSpace(intent.ClarifyingQuestion)
	if question == "" {
		question = "\u6211\u4e0d\u592a\u786e\u5b9a\u8fd9\u662f\u95f2\u804a\u8fd8\u662f\u4e00\u4e2a\u8981\u5904\u7406\u7684\u4efb\u52a1\u3002\u4f60\u5e0c\u671b\u6211\u76f4\u63a5\u56de\u7b54\uff0c\u8fd8\u662f\u5f00\u59cb\u4e00\u4e2a\u4efb\u52a1\uff1f"
	}
	return true, api.MessageResponse{Identity: identity, Content: question}
}

func (d *Server) tryHandleDirectIntent(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, intent router.IntentResult) (bool, api.MessageResponse) {
	if identity == nil || intent.ShouldCreateTask || intent.Intent != router.IntentCasual || intent.ShouldUseTools || intent.Confidence < 0.8 {
		return false, api.MessageResponse{}
	}
	if d == nil || d.Gateway == nil {
		return true, api.MessageResponse{Identity: identity, Content: "SelfMind is running, but the model gateway is not configured."}
	}
	resp, err := d.Gateway.Handle(ctx, identity.PersonID, req.Channel, req.Content)
	if err != nil {
		return true, api.MessageResponse{Identity: identity, Error: err.Error()}
	}
	content, usage, err := aggregateDirectResponse(resp)
	if err != nil {
		return true, api.MessageResponse{Identity: identity, Error: err.Error()}
	}
	return true, api.MessageResponse{Identity: identity, Content: content, Usage: usage}
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

func (d *Server) withResumeContext(ctx context.Context, identity *control.IdentityContext, task *control.Task, run *control.Run, intent router.IntentResult, input string) string {
	if d == nil || d.Control == nil || task == nil || intent.Intent != router.IntentContinue {
		return input
	}
	handoff, _ := d.Control.LatestHandoff(ctx, task.ID)
	events, _ := d.Control.ListTaskEvents(ctx, task.ID, 8)
	if handoff == nil && len(events) == 0 {
		return input
	}
	runID := ""
	if run != nil {
		runID = run.ID
	}
	_, _ = d.Control.AppendEvent(ctx, control.Event{
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
