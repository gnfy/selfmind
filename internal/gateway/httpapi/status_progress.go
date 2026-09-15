package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"selfmind/internal/control"
)

// Progress is selected from this Run's typed events, never from a previous
// handoff or a model-authored summary of another execution attempt.
func (d *Server) activeProgress(ctx context.Context, identity *control.IdentityContext, active *activeRun) string {
	events, err := d.Control.ListRunEvents(ctx, identity.TenantID, identity.PersonID, active.TaskID, active.RunID, 50)
	if err != nil {
		return "Current activity unavailable."
	}
	for _, event := range events {
		var p struct {
			Tool     string `json:"tool"`
			ToolName string `json:"tool_name"`
			Phase    string `json:"phase"`
		}
		if json.Unmarshal(event.Payload, &p) != nil {
			continue
		}
		if p.Tool == "" {
			p.Tool = p.ToolName
		}
		label := ""
		switch event.Type {
		case "agent.thinking":
			switch p.Phase {
			case "model_wait":
				label = "Waiting for the model response"
			case "thinking", "tool_selection", "tool_budget":
				label = "Planning the next action"
			}
		case "tool.started", "tool.heartbeat", "tool.sandbox":
			label = "Running tool: " + truncate(toOneLine(p.Tool), 60)
		case "tool.completed":
			label = "Last completed tool: " + truncate(toOneLine(p.Tool), 60)
		}
		if label != "" {
			age := time.Since(event.CreatedAt).Round(time.Second)
			if age < 0 {
				age = 0
			}
			return fmt.Sprintf("Current activity: %s (updated %s ago).", label, age)
		}
	}
	return "Current activity: starting this run."
}
