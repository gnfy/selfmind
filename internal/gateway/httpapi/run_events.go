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
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/platform/log"
	"selfmind/internal/tools"
)

func (c *RunCoordinator) startRunHeartbeat(ctx context.Context, run *control.Run, queueID, claimToken string) func() {
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
				if queueID != "" && claimToken != "" {
					_, _ = store.RenewQueuedClaim(context.Background(), run.TenantID, queueID, claimToken, 0)
				}
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
	// Sender-aware delivery policy: never mint outbound rows that no platform
	// sender can deliver. CLI/system async runs in particular have no push
	// surface for a 30s progress stream (and forwarding progress ticks to the
	// preferred IM would violate the IM cadence contract — concise notices and
	// final results only); their FINAL result already routes to the preferred
	// IM via deliverAsyncResult. Observed live 2026-07-18: 163 progress rows
	// for platform "cli" all failed with "no sender for platform cli".
	if !c.srv.Delivery.SupportsPlatform(req.Platform) {
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

func (c *RunCoordinator) deliverAsyncResult(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, resp api.MessageResponse) bool {
	if c == nil || c.srv == nil || c.srv.Delivery == nil || identity == nil {
		return false
	}
	base := delivery.Message{
		TenantID:   identity.TenantID,
		PersonID:   identity.PersonID,
		TaskID:     taskIDForResponse(resp),
		RunID:      runIDForResponse(resp),
		Kind:       delivery.KindFinalResult,
		Content:    deliveryContent(resp),
		LogicalKey: strings.TrimSpace(req.EffectKey),
	}
	accepted := false
	// Platform-only check: TUI channels are session UUIDs, not "cli".
	if req.Platform == "cli" {
		// A terminal has no push surface for a fire-and-forget run, so the
		// final answer used to vanish (the user only saw the task status flip
		// and read it as "nothing happened" — observed live with a rejected
		// approval whose acknowledgment nobody could see). Deliver it to the
		// person's single preferred IM endpoint — never a fan-out to every
		// bound account (conversation-layer rule 4). Task/run identity stays in
		// delivery metadata; a mutable task title must not pollute the answer.
		accepted = c.deliverToPreferredIMAccepted(ctx, identity, base)
		if !accepted && c.preferredIMAccount(ctx, identity) == nil {
			// The atomic finalizer already wrote the result to channel_messages.
			// For a CLI-only user that transcript is the intended durable target.
			accepted = true
		}
	} else {
		base.Platform = req.Platform
		base.PlatformUserID = identity.PlatformUserID
		base.Channel = req.Channel
		accepted, _ = c.srv.Delivery.EnqueueAndTryAccepted(ctx, base)
	}
	if accepted && req.EffectKey != "" && c.srv.Control != nil {
		if err := c.srv.Control.MarkEffectDeliveryEnqueued(ctx, identity.TenantID, req.EffectKey); err != nil {
			return false
		}
	}
	return accepted
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
		Kind:           delivery.KindFinalResult,
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

func (c *RunCoordinator) aggregateGatewayResponse(ctx context.Context, channel string, task *control.Task, run *control.Run, resp *router.HandleResponse) (string, llm.UsageStats, router.EventSummary, bool, error) {
	if resp == nil {
		return "", llm.UsageStats{}, router.EventSummary{}, false, nil
	}
	if !resp.IsStreaming {
		return resp.Content, resp.Usage, router.EventSummary{}, strings.TrimSpace(resp.Content) != "", nil
	}
	var finalContent strings.Builder
	var usage llm.UsageStats
	sawStream := false
	hasFinalContent := false
	typedAssistantPhase := false
	currentAssistantPhase := llm.AssistantPhaseUnspecified
	var summary router.EventSummary
	observer := streamObserverFromContext(ctx)
	for event := range resp.Stream {
		if event.EventType != "" {
			// Assistant prose emitted before a tool call is progress narration,
			// not the final answer. Keep publishing it live, but only materialize
			// prose produced after the last tool starts as the run's answer.
			if event.EventType == "tool.started" {
				finalContent.Reset()
				hasFinalContent = false
				typedAssistantPhase = false
				currentAssistantPhase = llm.AssistantPhaseUnspecified
			}
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
				if event.Phase != llm.AssistantPhaseUnspecified {
					if event.Phase == llm.AssistantPhaseFinalAnswer && currentAssistantPhase != llm.AssistantPhaseFinalAnswer {
						finalContent.Reset()
						hasFinalContent = false
					}
					typedAssistantPhase = true
					currentAssistantPhase = event.Phase
				}
				materialize := !typedAssistantPhase || currentAssistantPhase == llm.AssistantPhaseFinalAnswer
				if materialize {
					finalContent.WriteString(event.Content)
					if strings.TrimSpace(event.Content) != "" {
						hasFinalContent = true
					}
				}
			}
			if event.Usage != nil {
				usage = *event.Usage
			}
			continue
		}
		if event.Err != nil {
			return finalContent.String(), usage, summary, hasFinalContent, event.Err
		}
		if event.Content != "" && !sawStream {
			streamEvent := llm.StreamEvent{EventType: "stream", Content: event.Content}
			if task != nil {
				c.srv.events().publishAssistant(task, run, streamEvent)
			}
			if observer != nil {
				observer(streamEvent)
			}
			finalContent.WriteString(event.Content)
			if strings.TrimSpace(event.Content) != "" {
				hasFinalContent = true
			}
		}
		if event.Usage != nil {
			usage = *event.Usage
		}
	}
	return summary.WithContent(finalContent.String()), usage, summary, hasFinalContent, nil
}

func (c *RunCoordinator) recordStreamEvent(ctx context.Context, channel string, task *control.Task, run *control.Run, event llm.StreamEvent) {
	if c == nil || c.srv == nil || c.srv.Control == nil || task == nil {
		return
	}
	eventType := event.EventType
	// agent.step is the per-iteration state-machine trace (P0-B): live
	// observability only, never durable task history — persisting one row per
	// loop iteration would bloat task_events for no recall value.
	if eventType == "stream" || eventType == "" || eventType == "tool.heartbeat" || eventType == "tool.output" || eventType == "agent.step" {
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
		if plan := workUnitPlanFromPayload(event.Payload); run != nil && len(plan) > 0 {
			if units, err := c.srv.Control.SyncRunWorkUnits(ctx, task.TenantID, run.ID, plan); err != nil {
				log.Warn("work-unit plan projection failed", "run_id", run.ID, "error", err)
			} else {
				compact := make([]map[string]interface{}, 0, len(units))
				for _, unit := range units {
					compact = append(compact, map[string]interface{}{
						"id": unit.ID, "sequence": unit.Sequence, "status": unit.Status,
						"plan_status": unit.PlanStatus, "related_task_id": unit.RelatedTaskID,
					})
				}
				payload["work_units"] = compact
			}
		}
	case "run.outcome":
		eventType = "run.outcome"
	case "agent.steering":
		// run.steered means the gateway accepted guidance. This separate durable
		// event proves the agent actually folded it into the next model request.
		eventType = "run.steering_consumed"
		// Consumption proof for the persistent mailbox: new kernels carry the
		// exact mailbox ID. Fall back to the content hash only for events emitted
		// by an older process during a rolling upgrade.
		if run != nil {
			var consumed bool
			var err error
			if id, _ := payload["steering_id"].(string); strings.TrimSpace(id) != "" {
				consumed, err = c.srv.Control.ConsumeSteeringByID(ctx, task.TenantID, run.ID, id)
			} else if hash, _ := payload["content_hash"].(string); strings.TrimSpace(hash) != "" {
				consumed, err = c.srv.Control.ConsumeSteeringByHash(ctx, task.TenantID, run.ID, hash)
			}
			if err != nil {
				log.Warn("steering consumption mark failed", "run_id", run.ID, "error", err)
			} else if !consumed {
				log.Warn("steering consumption proof did not match a live mailbox row", "run_id", run.ID)
			}
		}
		delete(payload, "input")
		payload["message"] = "Mid-turn guidance was applied to the next model step."
	case "agent.thinking", "agent.step", "strategy.selected", "turn.started", "turn.completed", "token.updated", "provider.call.usage", "provider.call.context_breakdown":
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

func workUnitPlanFromPayload(payload map[string]interface{}) []control.WorkUnitPlanInput {
	if payload == nil {
		return nil
	}
	switch raw := payload["plan"].(type) {
	case []kernel.PlanItem:
		steps := make([]workUnitProjectionStep, 0, len(raw))
		for _, item := range raw {
			steps = append(steps, workUnitProjectionStep{
				Step: item.Step, Status: item.Status, RelatedTaskID: item.RelatedTaskID,
				WorkUnitID: item.WorkUnitID, WorkUnit: item.WorkUnit,
			})
		}
		return projectWorkUnitPlan(steps)
	case []interface{}:
		steps := make([]workUnitProjectionStep, 0, len(raw))
		for _, item := range raw {
			row, _ := item.(map[string]interface{})
			related, _ := row["related_task_id"].(string)
			workUnitID, _ := row["work_unit_id"].(string)
			workUnit, _ := row["work_unit"].(bool)
			steps = append(steps, workUnitProjectionStep{
				Step: fmt.Sprintf("%v", row["step"]), Status: fmt.Sprintf("%v", row["status"]),
				RelatedTaskID: related, WorkUnitID: workUnitID, WorkUnit: workUnit,
			})
		}
		return projectWorkUnitPlan(steps)
	default:
		return nil
	}
}

type workUnitProjectionStep struct {
	Step          string
	Status        string
	RelatedTaskID string
	WorkUnitID    string
	WorkUnit      bool
}

// projectWorkUnitPlan keeps ordinary plan refinements inside one execution
// attribution boundary. A new unit begins only at the first step, an explicit
// work_unit marker, a stable returned id, or a deterministically related task.
func projectWorkUnitPlan(steps []workUnitProjectionStep) []control.WorkUnitPlanInput {
	if len(steps) == 0 {
		return nil
	}
	boundaries := []int{0}
	for i := 1; i < len(steps); i++ {
		if steps[i].WorkUnit || strings.TrimSpace(steps[i].WorkUnitID) != "" || strings.TrimSpace(steps[i].RelatedTaskID) != "" {
			boundaries = append(boundaries, i)
		}
	}
	out := make([]control.WorkUnitPlanInput, 0, len(boundaries))
	for i, start := range boundaries {
		end := len(steps)
		if i+1 < len(boundaries) {
			end = boundaries[i+1]
		}
		out = append(out, control.WorkUnitPlanInput{
			WorkUnitID: strings.TrimSpace(steps[start].WorkUnitID), GoalDigest: strings.TrimSpace(steps[start].Step),
			PlanStatus: aggregateWorkUnitPlanStatus(steps[start:end]), RelatedTaskID: strings.TrimSpace(steps[start].RelatedTaskID),
		})
	}
	return out
}

func aggregateWorkUnitPlanStatus(steps []workUnitProjectionStep) string {
	hasPending, hasCompleted := false, false
	for _, step := range steps {
		switch strings.TrimSpace(step.Status) {
		case "in_progress":
			return "in_progress"
		case "pending":
			hasPending = true
		case "completed":
			hasCompleted = true
		}
	}
	if hasPending {
		return "pending"
	}
	if hasCompleted {
		return "completed"
	}
	return "cancelled"
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
