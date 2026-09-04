package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/platform/textutil"
)

// selectedTaskRuntimeContext assembles the bounded durable-context slice for
// one turn. userMessage is the ORIGINAL user content (before daemon/workspace/
// resume wrapping) — it feeds the automatic recall query and is never itself
// injected anywhere.
//
// preLabel marks a soft pre-label GUESS (ordinary message, no explicit
// continuation evidence — see resolveTask/taskAttach). For those turns the
// guessed task must NOT bias the prompt: only minimal label metadata
// (id/title/status) is injected, and the rich slices (summary, handoff,
// events, artifacts, next steps) are withheld — the spine tail carries the
// live work, and semantic recall (whose current-task exclusion is lifted for
// pre-label turns) surfaces related prior work with an explicit
// "possibly related; reference only" framing instead. Explicit attaches
// (/resume, task_id, continuation cue) keep the full context.
func (c *RunCoordinator) selectedTaskRuntimeContext(ctx context.Context, task *control.Task, run *control.Run, workspace *control.Workspace, platform, channel, userMessage string, preLabel bool) kernel.TaskRuntimeContext {
	mode := attachContextFull
	var parent *control.Run
	if preLabel {
		mode = attachContextNone
	} else if c != nil && c.srv != nil && c.srv.Control != nil && task != nil {
		// Direct callers (tests, non-coordinator paths) resolve the parent here;
		// runMessage resolves it once up front and passes it explicitly.
		identity := &control.IdentityContext{TenantID: task.TenantID, PersonID: task.PersonID}
		if resolved, err := c.resolveResumeTarget(ctx, identity, task); err == nil {
			parent = resolved.exact()
		}
	}
	return c.selectedTaskRuntimeContextWithMode(ctx, task, run, workspace, platform, channel, userMessage, mode, parent)
}

// selectedTaskRuntimeContextWithMode assembles the runtime slice under the
// P0 continuity contract: full task context exists only run-scoped, keyed by
// the resolved parent run. A full-mode attach WITHOUT an exact parent is
// downgraded to the bounded task card (summary/next steps/blockers — no
// handoff, no artifacts, no events), so a mixed-history task can never leak an
// unrelated run's slice into a new run's prompt.
func (c *RunCoordinator) selectedTaskRuntimeContextWithMode(ctx context.Context, task *control.Task, run *control.Run, workspace *control.Workspace, platform, channel, userMessage string, mode attachContextMode, parent *control.Run) kernel.TaskRuntimeContext {
	if c == nil || c.srv == nil || c.srv.Control == nil || task == nil {
		return kernel.TaskRuntimeContext{}
	}
	requestedMode := mode
	if mode == attachContextFull && parent == nil {
		mode = attachContextBounded
	}
	if parent != nil && parent.TaskID != task.ID {
		// A parent from another task is a caller bug; refuse rather than leak.
		parent = nil
		if mode == attachContextFull {
			mode = attachContextBounded
		}
	}
	includeTask := mode == attachContextBounded || mode == attachContextFull
	includeFull := mode == attachContextFull
	c.appendContextScopeEvent(ctx, task, run, channel, requestedMode, mode, parent)
	selected := kernel.TaskRuntimeContext{
		TaskID:      task.ID,
		Title:       task.Title,
		Status:      task.Status,
		Channel:     fallback(channel, task.LastChannel),
		WorkspaceID: recoveryWorkspaceID(run, task),
	}
	if includeTask {
		selected.Summary = task.CurrentSummary
		selected.NextSteps = append([]string{}, task.NextSteps...)
	}
	if run != nil {
		selected.RunID = run.ID
		if selected.Channel == "" {
			selected.Channel = run.Channel
		}
		if selected.WorkspaceID == "" {
			selected.WorkspaceID = run.WorkspaceID
		}
	}
	// Backward-compat read key: the channel of the parent run this turn
	// continues. A task created before working history became task-keyed stored
	// its transcript channel-keyed; without this the first task-keyed
	// continuation would look amnesiac. Run-scoped by P0: only the exact parent
	// may supply it.
	if includeFull && parent != nil {
		selected.PriorChannel = parent.Channel
	}
	if workspace != nil {
		selected.WorkspaceID = firstNonEmptyString(selected.WorkspaceID, workspace.ID)
		selected.Workspace = workspace.LocalPath
	}
	if includeFull && parent != nil {
		if handoff, _ := c.srv.Control.RunHandoff(ctx, task.TenantID, task.PersonID, parent.ID); handoff != nil {
			selected.Handoff = &kernel.TaskHandoffContext{
				Summary:      handoff.Summary,
				DoneItems:    append([]string{}, handoff.DoneItems...),
				NextSteps:    append([]string{}, handoff.NextSteps...),
				ChangedFiles: append([]string{}, handoff.ChangedFiles...),
				TestStatus:   handoff.TestStatus,
				Risks:        append([]string{}, handoff.Risks...),
				CreatedAt:    handoff.CreatedAt,
			}
			if selected.Summary == "" {
				selected.Summary = handoff.Summary
			}
			if len(selected.NextSteps) == 0 {
				selected.NextSteps = append([]string{}, handoff.NextSteps...)
			}
		}
	}
	if includeFull && parent != nil {
		if artifacts, _ := c.srv.Control.ListRunArtifacts(ctx, task.TenantID, task.PersonID, task.ID, parent.ID, 6); len(artifacts) > 0 {
			selected.Artifacts = make([]kernel.TaskArtifactContext, 0, len(artifacts))
			for _, artifact := range artifacts {
				selected.Artifacts = append(selected.Artifacts, kernel.TaskArtifactContext{
					ID:        artifact.ID,
					Kind:      artifact.Kind,
					Name:      artifact.Name,
					URI:       artifact.URI,
					MimeType:  artifact.MimeType,
					Summary:   artifactMetadataSummary(artifact.Metadata),
					CreatedAt: artifact.CreatedAt,
				})
			}
		}
	}
	// Fetch a larger candidate window, then keep the most relevant events
	// within the budget (W3d) rather than just the most recent 8. Run-scoped
	// by P0: only the exact parent run's events qualify.
	if includeFull && parent != nil {
		if events, _ := c.srv.Control.ListRunEvents(ctx, task.TenantID, task.PersonID, task.ID, parent.ID, 40); len(events) > 0 {
			ranked := rankTaskEvents(events, 8)
			selected.Events = make([]kernel.TaskEventContext, 0, len(ranked))
			for _, event := range ranked {
				selected.Events = append(selected.Events, kernel.TaskEventContext{
					Type:      event.Type,
					Channel:   event.Channel,
					Summary:   eventPayloadSummary(event.Payload),
					CreatedAt: event.CreatedAt,
				})
			}
		}
	}
	// A fresh IM inbound already triggers channel-specific catch-up. CLI cannot
	// refresh an IM context token, so instead give the agent a small advisory
	// that the prior final answer may need to be restated on this endpoint.
	if includeFull && strings.EqualFold(strings.TrimSpace(platform), "cli") {
		if pushes, err := c.srv.Control.ListUndeliveredTaskResults(ctx, task.TenantID, task.PersonID, task.ID, time.Now().Add(-24*time.Hour), 3); err == nil {
			for _, push := range pushes {
				preview := textutil.Truncate(strings.TrimSpace(push.Content), 180)
				selected.DeliveryWarnings = append(selected.DeliveryWarnings,
					fmt.Sprintf("%s result status=%s, updated=%s, preview=%q", push.Platform, push.Status, push.UpdatedAt.Format(time.RFC3339), preview))
			}
		}
	}
	// Automatic semantic recall (Work Timeline P2): bounded cross-history
	// slices selected at the SELECTOR layer and attached to the runtime
	// context for this turn only — the render path is TaskRuntimeContext →
	// RuntimeContextBundle → system prompt, never the messages array, so
	// recall is ephemeral and absent from persisted working history. The
	// engine owns its own skip conditions (control-command-shaped or trivially
	// short input) and never fails the turn. The current task's own work line
	// is excluded — its context is already in this bundle and in the
	// task-keyed working history. Runs after event selection so this turn's
	// context.recall event is not echoed back into its own context.
	if c.srv.Recall != nil {
		// Refresh the deterministic procedural-knowledge projection from the
		// same authorized convention files used by the agent prompt. Indexing is
		// fail-open and never changes workspace or permission authority.
		c.refreshWorkspaceKnowledge(ctx, task, workspace)
		// Pre-label turns lift the current-task exclusion: their bundle above
		// is metadata-only, so the guessed task's own card must be allowed to
		// surface via recall when the message content actually relates to it.
		excludeTaskID := task.ID
		if mode == attachContextNone {
			excludeTaskID = ""
		}
		recallWorkspaceID := selected.WorkspaceID
		if workspace != nil && strings.TrimSpace(workspace.ID) != "" {
			recallWorkspaceID = workspace.ID
		}
		excludeRunID := ""
		if run != nil {
			excludeRunID = run.ID
		}
		slices, stats := c.srv.Recall.SelectForWorkspace(
			ctx,
			task.TenantID,
			task.PersonID,
			recallWorkspaceID,
			excludeTaskID,
			excludeRunID,
			userMessage,
		)
		selected.RecallSlices = slices
		runID := ""
		if run != nil {
			runID = run.ID
		}
		// Redacted observability: source counts + refs only, no excerpts, so
		// /events, /diag context, and eval can see what recall did without
		// leaking prior-session text into the event log. Zero-hit and skipped
		// turns emit too — "recall found nothing" and "recall never ran" are
		// different diagnoses (W2).
		payload := map[string]interface{}{
			"candidates": stats.Candidates,
			"sources":    stats.Sources,
			"refs":       stats.Refs,
			"expanded":   stats.Expanded,
			"terms":      stats.Terms,
			"slices":     len(slices),
			"elapsed_ms": stats.ElapsedMS,
		}
		if stats.Skipped != "" {
			payload["skipped"] = stats.Skipped
		}
		_, _ = c.srv.Control.AppendEvent(ctx, control.Event{
			TaskID:     task.ID,
			RunID:      runID,
			Type:       "context.recall",
			Visibility: "task",
			Channel:    fallback(channel, task.LastChannel),
			Payload:    mustJSON(payload),
		})
	}
	return selected
}

// workContinuityHints supplies Main with a small deterministic Attention view
// before it interprets an otherwise new natural-language turn. This closes the
// short-reply gap without an ingress classifier or an extra model call:
// semantic recall may skip "确认执行", while the waiting Run card is still in
// the normal Main context. The current interaction is excluded and every card
// remains person-scoped and transcript-free.
func (c *RunCoordinator) workContinuityHints(ctx context.Context, identity *control.IdentityContext, current *control.Run, channel string, limit int) []kernel.WorkContinuityHint {
	if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil || current == nil || limit <= 0 {
		return nil
	}
	items, err := control.NewWorkTimeline(c.srv.Control).AttentionForChannel(
		ctx, identity.TenantID, identity.PersonID, strings.TrimSpace(channel), limit+1,
	)
	if err != nil {
		return nil
	}
	hints := make([]kernel.WorkContinuityHint, 0, min(limit, len(items)))
	for _, item := range items {
		if item.RunID == current.ID {
			continue
		}
		run, err := c.srv.Control.GetRun(ctx, identity.TenantID, item.RunID)
		if err != nil || run == nil || run.PersonID != identity.PersonID {
			continue
		}
		card, ok := c.srv.continuityCandidateForRun(ctx, identity, *run, c.currentActive(identity.PersonID), 0, []string{"attention_hint"})
		if !ok {
			continue
		}
		hints = append(hints, kernel.WorkContinuityHint{
			RunID: card.RunID, TaskID: card.TaskID, Title: card.Title, RunStatus: card.RunStatus,
			Channel: card.Channel, Workspace: card.Workspace, InputSummary: card.InputSummary,
			HandoffSummary: card.HandoffSummary, CurrentStep: card.CurrentStep,
			NextSteps: append([]string(nil), card.NextSteps...),
		})
		if len(hints) == limit {
			break
		}
	}
	return hints
}

// appendContextScopeEvent records, per selector call, which context depth the
// turn actually received: the requested attach mode, the effective mode after
// the parent gate, and the parent run (if any). Redacted and structured — it
// is the observability row behind the `turn_binding_decision` metric and the
// deterministic eval assertions for P0.
func (c *RunCoordinator) appendContextScopeEvent(ctx context.Context, task *control.Task, run *control.Run, channel string, requested, effective attachContextMode, parent *control.Run) {
	if c == nil || c.srv == nil || c.srv.Control == nil || task == nil {
		return
	}
	runID := ""
	if run != nil {
		runID = run.ID
	}
	payload := map[string]interface{}{
		"requested_mode": string(requested),
		"mode":           string(effective),
	}
	if parent != nil {
		payload["resumes_run_id"] = parent.ID
	} else if requested == attachContextFull {
		payload["downgraded"] = true
	}
	_, _ = c.srv.Control.AppendEvent(ctx, control.Event{
		TaskID:     task.ID,
		RunID:      runID,
		Type:       "context.scope",
		Visibility: "task",
		Channel:    fallback(channel, task.LastChannel),
		Payload:    mustJSON(payload),
	})
}

func reverseEvents(events []control.Event) []control.Event {
	out := make([]control.Event, len(events))
	for i := range events {
		out[i] = events[len(events)-1-i]
	}
	return out
}

func eventPayloadSummary(raw json.RawMessage) string {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "{}" || string(raw) == "null" {
		return ""
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err == nil && len(obj) > 0 {
		for _, key := range []string{"message", "summary", "status", "tool", "result", "error", "input"} {
			if value, ok := obj[key]; ok {
				return textutil.Truncate(toContextLine(value), 240)
			}
		}
		if outcome, ok := obj["outcome"]; ok {
			return textutil.Truncate(toContextLine(outcome), 240)
		}
		return textutil.Truncate(toContextLine(obj), 240)
	}
	return textutil.Truncate(string(raw), 240)
}

func artifactMetadataSummary(raw json.RawMessage) string {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "{}" || string(raw) == "null" {
		return ""
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err == nil && len(obj) > 0 {
		for _, key := range []string{"summary", "source", "description"} {
			if value, ok := obj[key]; ok {
				return textutil.Truncate(toContextLine(value), 180)
			}
		}
		return textutil.Truncate(toContextLine(obj), 180)
	}
	return textutil.Truncate(string(raw), 180)
}

func toContextLine(value interface{}) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case []string:
		return strings.Join(v, "; ")
	case []interface{}:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			parts = append(parts, toContextLine(item))
		}
		return strings.Join(parts, "; ")
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(data)
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
