package kernel

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"selfmind/internal/kernel/llm"
)

const agentEventPrefix = "event:"

var (
	agentEventRedactorMu sync.RWMutex
	agentEventRedactor   func(string) string
)

// SetAgentEventRedactor installs the output-boundary redactor without making
// kernel depend on a concrete tools implementation.
func SetAgentEventRedactor(redactor func(string) string) {
	agentEventRedactorMu.Lock()
	agentEventRedactor = redactor
	agentEventRedactorMu.Unlock()
}

type PlanItem struct {
	StepID               string `json:"step_id,omitempty"`
	Step                 string `json:"step"`
	Status               string `json:"status"`
	SuccessCriteria      string `json:"success_criteria,omitempty"`
	VerificationRequired bool   `json:"verification_required,omitempty"`
	RelatedTaskID        string `json:"related_task_id,omitempty"`
	WorkUnitID           string `json:"work_unit_id,omitempty"`
	WorkUnit             bool   `json:"work_unit,omitempty"`
}

type AgentEvent struct {
	Type            string                 `json:"type"`
	Content         string                 `json:"content,omitempty"`
	Phase           llm.AssistantPhase     `json:"phase,omitempty"`
	ToolName        string                 `json:"tool_name,omitempty"`
	ToolCallID      string                 `json:"tool_call_id,omitempty"`
	ToolArgs        string                 `json:"tool_args,omitempty"`
	ToolResult      string                 `json:"tool_result,omitempty"`
	DurationSeconds float64                `json:"duration_seconds,omitempty"`
	Error           string                 `json:"error,omitempty"`
	Plan            []PlanItem             `json:"plan,omitempty"`
	Payload         map[string]interface{} `json:"payload,omitempty"`
}

func EncodeAgentEvent(event AgentEvent) string {
	event = redactAgentEvent(event)
	data, err := json.Marshal(event)
	if err != nil {
		return agentEventPrefix + `{"type":"agent.event","error":"encode failed"}`
	}
	return agentEventPrefix + string(data)
}

func redactAgentEvent(event AgentEvent) AgentEvent {
	agentEventRedactorMu.RLock()
	redactor := agentEventRedactor
	agentEventRedactorMu.RUnlock()
	if redactor == nil {
		return event
	}
	event.Content = redactor(event.Content)
	event.ToolArgs = redactor(event.ToolArgs)
	event.ToolResult = redactor(event.ToolResult)
	event.Error = redactor(event.Error)
	event.Payload = redactEventMap(event.Payload, redactor)
	return event
}

func redactEventMap(input map[string]interface{}, redactor func(string) string) map[string]interface{} {
	if len(input) == 0 {
		return input
	}
	out := make(map[string]interface{}, len(input))
	for key, value := range input {
		switch typed := value.(type) {
		case string:
			out[key] = redactor(typed)
		case map[string]interface{}:
			out[key] = redactEventMap(typed, redactor)
		case []interface{}:
			items := make([]interface{}, len(typed))
			for i, item := range typed {
				if text, ok := item.(string); ok {
					items[i] = redactor(text)
				} else {
					items[i] = item
				}
			}
			out[key] = items
		default:
			out[key] = value
		}
	}
	return out
}

func DecodeAgentEvent(raw string) (AgentEvent, bool) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, agentEventPrefix) {
		return AgentEvent{}, false
	}
	var event AgentEvent
	if err := json.Unmarshal([]byte(strings.TrimPrefix(raw, agentEventPrefix)), &event); err != nil {
		return AgentEvent{}, false
	}
	if event.Type == "" {
		event.Type = "agent.event"
	}
	return event, true
}

func EmitAgentEvent(ch chan string, event AgentEvent) {
	if ch == nil {
		return
	}
	encoded := EncodeAgentEvent(event)
	select {
	case ch <- encoded:
	default:
		if !isCriticalAgentEvent(event.Type) {
			return
		}
		timer := time.NewTimer(50 * time.Millisecond)
		defer timer.Stop()
		select {
		case ch <- encoded:
		case <-timer.C:
		}
	}
}

func isCriticalAgentEvent(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "stream", "tool.started", "tool.completed", "tool.output", "turn.completed", "token.updated", "provider.call.usage", "provider.call.context_breakdown", "evidence.recorded":
		return true
	default:
		return false
	}
}
