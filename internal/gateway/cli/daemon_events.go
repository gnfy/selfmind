package cli

import (
	"encoding/json"
	"strings"
	"time"

	"selfmind/internal/gateway/api"
)

func eventRefFromRunEvent(event api.RunEvent) uiEventRef {
	return uiEventRef{
		Source:  eventSourceDaemon,
		RunID:   strings.TrimSpace(event.RunID),
		EventID: strings.TrimSpace(event.EventID),
		Cursor:  event.Cursor,
		LiveSeq: event.LiveSeq,
	}
}

func (m *uiModel) forwardDaemonRunEvent(event api.RunEvent) {
	if m == nil || m.program == nil {
		return
	}
	switch event.Type {
	case "run.started":
		var payload struct {
			Input string `json:"input"`
		}
		_ = json.Unmarshal(event.Payload, &payload)
		started := event.CreatedAt
		if started.IsZero() {
			started = time.Now()
		}
		m.program.Send(MsgDaemonRunStarted{
			RunID:   strings.TrimSpace(event.RunID),
			TaskID:  strings.TrimSpace(event.TaskID),
			Input:   strings.TrimSpace(payload.Input),
			Started: started,
			Event:   eventRefFromRunEvent(event),
		})
	case "run.finished", "run.cancelled", "run.interrupted", "run.failed":
		var payload struct {
			Outcome api.RunOutcome `json:"outcome"`
		}
		_ = json.Unmarshal(event.Payload, &payload)
		status := strings.TrimSpace(payload.Outcome.Status)
		if status == "" {
			switch event.Type {
			case "run.finished":
				status = "done"
			case "run.cancelled":
				status = "cancelled"
			default:
				status = "error"
			}
		}
		m.program.Send(MsgDaemonRunFinished{
			RunID:   strings.TrimSpace(event.RunID),
			Status:  status,
			Summary: strings.TrimSpace(payload.Outcome.Summary),
			Event:   eventRefFromRunEvent(event),
		})
	}
}

func sameQueuedInput(full, eventInput string) bool {
	full = strings.TrimSpace(full)
	eventInput = strings.TrimSpace(eventInput)
	if full == "" || eventInput == "" {
		return false
	}
	return full == eventInput || strings.HasPrefix(full, eventInput) || strings.HasPrefix(eventInput, full)
}

func (m *uiModel) consumeQueuedInput(eventInput string) bool {
	for i, input := range m.queuedInputs {
		if !sameQueuedInput(input, eventInput) {
			continue
		}
		m.queuedInputs = append(m.queuedInputs[:i], m.queuedInputs[i+1:]...)
		if m.queuedCount > 0 {
			m.queuedCount--
		}
		return true
	}
	return false
}

func uiStatusForDaemonOutcome(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "done", "completed", "succeeded", "success":
		return "done"
	case "cancelled", "canceled":
		return "cancelled"
	case "running", "waiting_user", "waiting_external", "verification_partial", "interrupted":
		return "done"
	default:
		return "error"
	}
}

func (m *uiModel) passiveDaemonEvent(ref uiEventRef) bool {
	if ref.Source != eventSourceDaemon {
		return false
	}
	if m.daemonRunID != "" && ref.RunID != "" && ref.RunID != m.daemonRunID {
		return true
	}
	return !m.daemonRunAwaitingDone
}

func queuedTurn(turn *api.TurnStatus) bool {
	return turn != nil && strings.EqualFold(strings.TrimSpace(turn.Status), "queued")
}
