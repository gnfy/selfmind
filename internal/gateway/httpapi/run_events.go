package httpapi

import (
	"context"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/tools"
)

func (c *RunCoordinator) startRunHeartbeat(ctx context.Context, run *control.Run) func() {
	if c == nil || c.srv == nil || c.srv.Control == nil || run == nil {
		return func() {}
	}
	store := c.srv.Control
	done := make(chan struct{})
	go func() {
		// runHeartbeatInterval is shared with the stuck-run sweeper's staleness
		// threshold (run_recovery.go); change them together.
		ticker := time.NewTicker(runHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = store.UpdateRunHeartbeat(context.Background(), run.TenantID, run.ID)
			}
		}
	}()
	return func() {
		close(done)
		_ = store.UpdateRunHeartbeat(context.Background(), run.TenantID, run.ID)
	}
}

func (c *RunCoordinator) startAsyncProgressNotices(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest) func() {
	if c == nil || c.srv == nil || c.srv.Delivery == nil || identity == nil || router.ShouldStreamToClient(req.Channel) {
		return func() {}
	}
	deliver := c.srv.Delivery
	done := make(chan struct{})
	start := time.Now()
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				elapsed := time.Since(start).Round(time.Second)
				content := fmt.Sprintf("SelfMind is still working (%s elapsed). I will send the result here when it finishes.", elapsed)
				_ = deliver.EnqueueAndTry(context.Background(), delivery.Message{
					TenantID:       identity.TenantID,
					PersonID:       identity.PersonID,
					Platform:       req.Platform,
					PlatformUserID: identity.PlatformUserID,
					Channel:        req.Channel,
					Content:        content,
				})
			}
		}
	}()
	return func() {
		close(done)
	}
}

func (c *RunCoordinator) deliverAsyncResult(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, resp api.MessageResponse) {
	if c == nil || c.srv == nil || c.srv.Delivery == nil || identity == nil {
		return
	}
	// Platform-only check: TUI channels are session UUIDs, not "cli".
	if req.Platform == "cli" {
		// A terminal has no push surface for a fire-and-forget run, so the
		// final answer used to vanish (the user only saw the task status flip
		// and read it as "nothing happened" — observed live with a rejected
		// approval whose acknowledgment nobody could see). Deliver it to the
		// person's single preferred IM endpoint — never a fan-out to every
		// bound account (conversation-layer rule 4). Task/run identity stays in
		// delivery metadata; a mutable task title must not pollute the answer.
		c.deliverToPreferredIM(ctx, identity, delivery.Message{
			TenantID: identity.TenantID,
			PersonID: identity.PersonID,
			TaskID:   taskIDForResponse(resp),
			RunID:    runIDForResponse(resp),
			Content:  deliveryContent(resp),
		})
		return
	}
	_ = c.srv.Delivery.EnqueueAndTry(ctx, delivery.Message{
		TenantID:       identity.TenantID,
		PersonID:       identity.PersonID,
		Platform:       req.Platform,
		PlatformUserID: identity.PlatformUserID,
		Channel:        req.Channel,
		TaskID:         taskIDForResponse(resp),
		RunID:          runIDForResponse(resp),
		Content:        deliveryContent(resp),
	})
}

// deliverCronResult pushes a scheduled job's result to its channel. Unlike the
// interactive async path it must skip a "busy" tick (the person was mid-run, so
// the job did not actually execute) instead of delivering a confusing notice.
func (c *RunCoordinator) deliverCronResult(ctx context.Context, req api.MessageRequest, resp api.MessageResponse) {
	if c == nil || c.srv == nil || c.srv.Delivery == nil {
		return
	}
	if resp.Turn != nil && resp.Turn.Status == "busy" {
		return // the run was skipped because the person was active; try again next tick
	}
	if req.Platform == "cli" {
		return // local-only job: nothing to push to a channel (platform-only: TUI channels are session UUIDs)
	}
	platformUser := req.PlatformUserID
	if resp.Identity != nil && resp.Identity.PlatformUserID != "" {
		platformUser = resp.Identity.PlatformUserID
	}
	tenantID, personID := req.TenantID, ""
	if resp.Identity != nil {
		tenantID = firstNonEmptyString(resp.Identity.TenantID, tenantID)
		personID = resp.Identity.PersonID
	}
	_ = c.srv.Delivery.EnqueueAndTry(ctx, delivery.Message{
		TenantID:       tenantID,
		PersonID:       personID,
		Platform:       req.Platform,
		PlatformUserID: platformUser,
		Channel:        req.Channel,
		TaskID:         taskIDForResponse(resp),
		RunID:          runIDForResponse(resp),
		Content:        deliveryContent(resp),
	})
}

// deliveryContent renders the user-facing text for an outbound delivery: a
// redacted failure line, a fallback for empty output, or the response content.
func deliveryContent(resp api.MessageResponse) string {
	if resp.Error != "" {
		// The failed task parks as interrupted (resumable), so give the two
		// ways out — retry or drop — instead of a dead-end error dump.
		return "SelfMind task failed: " + tools.RedactSensitive(resp.Error) +
			"\nReply \"continue\" to retry, or /cancel to drop the task."
	}
	content := strings.TrimSpace(resp.Content)
	if content == "" {
		return "SelfMind task finished."
	}
	return content
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

func (c *RunCoordinator) aggregateGatewayResponse(ctx context.Context, channel string, task *control.Task, run *control.Run, resp *router.HandleResponse) (string, llm.UsageStats, router.EventSummary, error) {
	if resp == nil {
		return "", llm.UsageStats{}, router.EventSummary{}, nil
	}
	if !resp.IsStreaming {
		return resp.Content, resp.Usage, router.EventSummary{}, nil
	}
	var content strings.Builder
	var usage llm.UsageStats
	sawStream := false
	var summary router.EventSummary
	observer := streamObserverFromContext(ctx)
	for event := range resp.Stream {
		if event.EventType != "" {
			if event.EventType == "stream" && task != nil {
				c.srv.events().publishAssistant(task, run, event)
			}
			if observer != nil {
				observer(event)
			}
			summary.Observe(event)
			c.recordStreamEvent(ctx, channel, task, run, event)
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
			return content.String(), usage, summary, event.Err
		}
		if event.Content != "" && !sawStream {
			streamEvent := llm.StreamEvent{EventType: "stream", Content: event.Content}
			if task != nil {
				c.srv.events().publishAssistant(task, run, streamEvent)
			}
			if observer != nil {
				observer(streamEvent)
			}
			content.WriteString(event.Content)
		}
		if event.Usage != nil {
			usage = *event.Usage
		}
	}
	return summary.WithContent(content.String()), usage, summary, nil
}

func (c *RunCoordinator) recordStreamEvent(ctx context.Context, channel string, task *control.Task, run *control.Run, event llm.StreamEvent) {
	if c == nil || c.srv == nil || c.srv.Control == nil || task == nil {
		return
	}
	eventType := event.EventType
	if eventType == "stream" || eventType == "" || eventType == "tool.heartbeat" {
		return
	}
	payload := map[string]interface{}{}
	if event.Payload != nil {
		for k, v := range event.Payload {
			payload[k] = v
		}
	}
	switch eventType {
	case "tool.started":
		payload["tool"] = event.ToolName
		if event.ToolCallID != "" {
			payload["tool_call_id"] = event.ToolCallID
		}
		payload["args"] = tools.RedactSensitive(event.ToolArgs)
	case "tool.completed":
		payload["tool"] = event.ToolName
		if event.ToolCallID != "" {
			payload["tool_call_id"] = event.ToolCallID
		}
		// Most tool results are previews (1000 chars is plenty). update_plan is the
		// exception: its result is the structured plan JSON that the client TUI
		// re-parses to render the Codex-style checklist, so a mid-JSON cut would
		// break rendering. Plans are bounded (≤20 short steps), so keep them whole.
		resultLimit := 1000
		if event.ToolName == "update_plan" {
			resultLimit = 8000
		}
		payload["result"] = tools.RedactSensitive(truncate(event.ToolResult, resultLimit))
		payload["duration_seconds"] = event.DurationSeconds
		if event.Err != nil {
			payload["error"] = tools.RedactSensitive(event.Err.Error())
		}
	case "learning.review":
		payload["message"] = tools.RedactSensitive(event.Content)
		eventType = classifyLearningReviewEvent(event.Content)
	case "plan.updated":
		eventType = "plan.updated"
	case "run.outcome":
		eventType = "run.outcome"
	case "agent.thinking", "agent.step", "strategy.selected", "turn.started", "turn.completed", "token.updated":
		if event.Content != "" {
			payload["message"] = tools.RedactSensitive(event.Content)
		}
	default:
		payload["message"] = tools.RedactSensitive(event.Content)
	}
	runID := ""
	if run != nil {
		runID = run.ID
	}
	_, _ = c.srv.Control.AppendEvent(ctx, control.Event{
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
