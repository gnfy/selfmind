package kernel

import (
	"encoding/json"
	"strings"
	"time"
)

const agentEventPrefix = "event:"

type PlanItem struct {
	Step   string `json:"step"`
	Status string `json:"status"`
}

type AgentEvent struct {
	Type            string                 `json:"type"`
	Content         string                 `json:"content,omitempty"`
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
	data, err := json.Marshal(event)
	if err != nil {
		return agentEventPrefix + `{"type":"agent.event","error":"encode failed"}`
	}
	return agentEventPrefix + string(data)
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
	case "stream", "tool.started", "tool.completed", "tool.output", "turn.completed", "token.updated", "evidence.recorded":
		return true
	default:
		return false
	}
}
