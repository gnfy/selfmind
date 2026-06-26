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

func (c *RunCoordinator) selectedTaskRuntimeContext(ctx context.Context, task *control.Task, run *control.Run, workspace *control.Workspace, channel string) kernel.TaskRuntimeContext {
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
	if events, _ := c.srv.Control.ListTaskEvents(ctx, task.ID, 8); len(events) > 0 {
		selected.Events = make([]kernel.TaskEventContext, 0, len(events))
		for _, event := range reverseEvents(events) {
			selected.Events = append(selected.Events, kernel.TaskEventContext{
				Type:      event.Type,
				Channel:   event.Channel,
				Summary:   eventPayloadSummary(event.Payload),
				CreatedAt: event.CreatedAt,
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
