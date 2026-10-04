package httpapi

import (
	"context"
	"encoding/json"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

// Origin records the trigger; presentation records whose interactive work is
// continuing. Only a durable same-session parent can transfer presentation.
func (c *RunCoordinator) runPresentation(ctx context.Context, run *control.Run, req api.MessageRequest) string {
	origin := runOrigin(ctx, req)
	if origin == "" {
		return "foreground"
	}
	for depth := 0; depth < 32; depth++ {
		switch origin {
		case runOriginProviderWait, runOriginResource, runOriginApproval, runOriginRecovery, "clarify":
		default:
			return "background"
		}
		if run.ResumesRunID == "" {
			return "background"
		}
		parent, err := c.srv.Control.GetRun(ctx, run.TenantID, run.ResumesRunID)
		if err != nil || parent == nil || parent.PersonID != run.PersonID || parent.Channel != run.Channel {
			return "background"
		}
		payload, err := c.srv.Control.RunStartedPayload(ctx, parent.TenantID, parent.PersonID, parent.ID)
		if err != nil || len(payload) == 0 {
			return "background"
		}
		var facts struct {
			Presentation string `json:"presentation"`
			Origin       string `json:"origin"`
		}
		if json.Unmarshal(payload, &facts) != nil {
			return "background"
		}
		if facts.Presentation == "foreground" || facts.Presentation == "background" {
			return facts.Presentation
		}
		// Historical Runs have an origin but no presentation. Follow their exact
		// lineage; a historical user-origin parent remains foreground.
		if facts.Origin == "" {
			return "foreground"
		}
		origin, run = facts.Origin, parent
	}
	return "background"
}

func (b *runEventBroker) withSavedAnswer(ctx context.Context, event api.RunEvent) api.RunEvent {
	if b == nil || b.store == nil || !isTerminalRunEvent(event.Type) || event.Channel == "" {
		return event
	}
	answer, err := b.store.RunAssistantContent(ctx, event.TenantID, event.PersonID, event.RunID)
	if err != nil || answer == "" {
		return event
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(event.Payload, &payload) != nil || payload == nil {
		return event
	}
	payload["final_answer"], _ = json.Marshal(answer)
	event.Payload, _ = json.Marshal(payload)
	return event
}
