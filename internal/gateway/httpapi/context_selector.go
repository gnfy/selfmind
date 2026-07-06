package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/platform/textutil"
)

// selectedTaskRuntimeContext assembles the bounded durable-context slice for
// one turn. userMessage is the ORIGINAL user content (before daemon/workspace/
// resume wrapping) — it feeds the automatic recall query and is never itself
// injected anywhere.
func (c *RunCoordinator) selectedTaskRuntimeContext(ctx context.Context, task *control.Task, run *control.Run, workspace *control.Workspace, channel, userMessage string) kernel.TaskRuntimeContext {
	if c == nil || c.srv == nil || c.srv.Control == nil || task == nil {
		return kernel.TaskRuntimeContext{}
	}
	selected := kernel.TaskRuntimeContext{
		TaskID:      task.ID,
		Title:       task.Title,
		Status:      task.Status,
		Summary:     task.CurrentSummary,
		Channel:     fallback(channel, task.LastChannel),
		WorkspaceID: task.WorkspaceID,
		NextSteps:   append([]string{}, task.NextSteps...),
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
	// Backward-compat read key: the channel of the task's most recent PRIOR run.
	// A task created before working history became task-keyed stored its
	// transcript channel-keyed; without this the first task-keyed continuation
	// would look amnesiac. StartRun already stamped task.LastChannel with the
	// current channel, so query the runs table for the previous run instead.
	exceptRunID := ""
	if run != nil {
		exceptRunID = run.ID
	}
	if prior, err := c.srv.Control.PriorRunChannel(ctx, task.TenantID, task.ID, exceptRunID); err == nil {
		selected.PriorChannel = prior
	}
	if workspace != nil {
		selected.WorkspaceID = firstNonEmptyString(selected.WorkspaceID, workspace.ID)
		selected.Workspace = workspace.LocalPath
	}
	if handoff, _ := c.srv.Control.LatestHandoff(ctx, task.ID); handoff != nil {
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
	if artifacts, _ := c.srv.Control.ListTaskArtifacts(ctx, task.ID, 6); len(artifacts) > 0 {
		selected.Artifacts = make([]kernel.TaskArtifactContext, 0, len(artifacts))
		for _, artifact := range artifacts {
			selected.Artifacts = append(selected.Artifacts, kernel.TaskArtifactContext{
				Kind:      artifact.Kind,
				Name:      artifact.Name,
				URI:       artifact.URI,
				MimeType:  artifact.MimeType,
				Summary:   artifactMetadataSummary(artifact.Metadata),
				CreatedAt: artifact.CreatedAt,
			})
		}
	}
	// Fetch a larger candidate window, then keep the most relevant events
	// within the budget (W3d) rather than just the most recent 8.
	if events, _ := c.srv.Control.ListTaskEvents(ctx, task.ID, 40); len(events) > 0 {
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
		if slices, stats := c.srv.Recall.Select(ctx, task.TenantID, task.PersonID, task.ID, userMessage); len(slices) > 0 {
			selected.RecallSlices = slices
			runID := ""
			if run != nil {
				runID = run.ID
			}
			// Redacted observability: source counts + refs only, no excerpts,
			// so /events and eval can see what recall did without leaking
			// prior-session text into the event log.
			_, _ = c.srv.Control.AppendEvent(ctx, control.Event{
				TaskID:     task.ID,
				RunID:      runID,
				Type:       "context.recall",
				Visibility: "task",
				Channel:    fallback(channel, task.LastChannel),
				Payload: mustJSON(map[string]interface{}{
					"sources":  stats.Sources,
					"refs":     stats.Refs,
					"expanded": stats.Expanded,
					"terms":    stats.Terms,
				}),
			})
		}
	}
	return selected
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
