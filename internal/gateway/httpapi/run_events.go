package httpapi

import (
	"context"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/tools"
)

func (d *Server) startRunHeartbeat(ctx context.Context, run *control.Run) func() {
	if d == nil || d.Control == nil || run == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = d.Control.UpdateRunHeartbeat(context.Background(), run.TenantID, run.ID)
			}
		}
	}()
	return func() {
		close(done)
		_ = d.Control.UpdateRunHeartbeat(context.Background(), run.TenantID, run.ID)
	}
}

func (d *Server) deliverAsyncResult(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, resp api.MessageResponse) {
	if d == nil || d.Delivery == nil || identity == nil {
		return
	}
	if req.Platform == "cli" && req.Channel == "cli" {
		return
	}
	content := strings.TrimSpace(resp.Content)
	if resp.Error != "" {
		content = "SelfMind task failed: " + tools.RedactSensitive(resp.Error)
	}
	if content == "" {
		content = "SelfMind task finished."
	}
	_ = d.Delivery.EnqueueAndTry(ctx, delivery.Message{
		TenantID:       identity.TenantID,
		PersonID:       identity.PersonID,
		Platform:       req.Platform,
		PlatformUserID: identity.PlatformUserID,
		Channel:        req.Channel,
		TaskID:         taskIDForResponse(resp),
		RunID:          runIDForResponse(resp),
		Content:        content,
	})
}

func taskIDForResponse(resp api.MessageResponse) string {
	if resp.Task == nil {
		return ""
	}
	return resp.Task.ID
}

func runIDForResponse(resp api.MessageResponse) string {
	if resp.Run == nil {
		return ""
	}
	return resp.Run.ID
}

func (d *Server) aggregateGatewayResponse(ctx context.Context, channel string, task *control.Task, run *control.Run, resp *router.HandleResponse) (string, llm.UsageStats, error) {
	if resp == nil {
		return "", llm.UsageStats{}, nil
	}
	if !resp.IsStreaming {
		return resp.Content, resp.Usage, nil
	}
	var content strings.Builder
	var usage llm.UsageStats
	sawStream := false
	for event := range resp.Stream {
		if event.EventType != "" {
			d.recordStreamEvent(ctx, channel, task, run, event)
			if event.EventType == "stream" {
				sawStream = true
				content.WriteString(event.Content)
			}
			if event.Usage != nil {
				usage = *event.Usage
			}
			continue
		}
		if event.Err != nil {
			return content.String(), usage, event.Err
		}
		if event.Content != "" && !sawStream {
			content.WriteString(event.Content)
		}
		if event.Usage != nil {
			usage = *event.Usage
		}
	}
	return content.String(), usage, nil
}

func (d *Server) recordStreamEvent(ctx context.Context, channel string, task *control.Task, run *control.Run, event llm.StreamEvent) {
	if d == nil || d.Control == nil || task == nil {
		return
	}
	eventType := event.EventType
	if eventType == "stream" || eventType == "" {
		return
	}
	payload := map[string]interface{}{}
	switch eventType {
	case "tool.started":
		payload["tool"] = event.ToolName
		payload["args"] = tools.RedactSensitive(event.ToolArgs)
	case "tool.completed":
		payload["tool"] = event.ToolName
		payload["result"] = tools.RedactSensitive(truncate(event.ToolResult, 1000))
		payload["duration_seconds"] = event.DurationSeconds
		if event.Err != nil {
			payload["error"] = tools.RedactSensitive(event.Err.Error())
		}
	case "learning.review":
		payload["message"] = tools.RedactSensitive(event.Content)
		eventType = classifyLearningReviewEvent(event.Content)
	default:
		payload["message"] = tools.RedactSensitive(event.Content)
	}
	runID := ""
	if run != nil {
		runID = run.ID
	}
	_, _ = d.Control.AppendEvent(ctx, control.Event{
		TaskID:     task.ID,
		RunID:      runID,
		Type:       eventType,
		Visibility: "task",
		Channel:    channel,
		Payload:    mustJSON(payload),
	})
}

func classifyLearningReviewEvent(content string) string {
	lower := strings.ToLower(strings.TrimSpace(content))
	switch {
	case strings.Contains(lower, "memory saved") || strings.Contains(lower, "added to memory"):
		return "learning.memory.saved"
	case strings.Contains(lower, "memory updated") || strings.Contains(lower, "memory replaced"):
		return "learning.memory.updated"
	case strings.Contains(lower, "skill created"):
		return "learning.skill.created"
	case strings.Contains(lower, "skill updated") || strings.Contains(lower, "skill patched") || strings.Contains(lower, "skill edited"):
		return "learning.skill.updated"
	case strings.Contains(lower, "nothing durable") || strings.Contains(lower, "nothing to save"):
		return "learning.skipped"
	case strings.Contains(lower, "skipped") || strings.Contains(lower, "failed"):
		return "learning.failed"
	default:
		return "learning.review"
	}
}
