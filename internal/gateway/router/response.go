package router

import (
	"encoding/json"
	"fmt"
	"strings"

	"selfmind/internal/kernel/llm"
)

// AggregateFinalResponse consumes a gateway response and returns one final
// message. Message-based channels should use this instead of forwarding stream
// chunks to users.
func AggregateFinalResponse(resp *HandleResponse) (string, llm.UsageStats, error) {
	if resp == nil {
		return "", llm.UsageStats{}, nil
	}
	if !resp.IsStreaming {
		return resp.Content, resp.Usage, nil
	}
	var content strings.Builder
	var usage llm.UsageStats
	sawStream := false
	var summary EventSummary
	for event := range resp.Stream {
		if event.Err != nil && event.EventType == "" {
			return content.String(), usage, event.Err
		}
		if event.EventType != "" {
			summary.Observe(event)
			if event.EventType == "stream" {
				sawStream = true
				content.WriteString(event.Content)
			}
			if event.Usage != nil {
				usage = *event.Usage
			}
			continue
		}
		if event.Content != "" && !sawStream {
			content.WriteString(event.Content)
		}
		if event.Usage != nil {
			usage = *event.Usage
		}
	}
	return summary.WithContent(content.String()), usage, nil
}

type EventSummary struct {
	phases       []string
	toolsStarted []string
	toolFailures []string
	lastOutputs  []string
	completion   TurnCompletion
}

// TurnCompletion captures the kernel's structured terminal signal. It is
// independent of assistant prose so a partial answer cannot be mistaken for a
// completed run.
type TurnCompletion struct {
	Status           string
	CompletionReason string
	FinishReason     string
	Resumable        bool
}

func (s *EventSummary) Observe(event llm.StreamEvent) {
	switch event.EventType {
	case "agent.thinking", "agent.step":
		line := strings.TrimSpace(event.Content)
		if line != "" {
			s.phases = appendLimited(s.phases, line, 6)
		}
	case "tool.started":
		label := event.ToolName
		if detail := toolArgsDetail(event.ToolArgs); detail != "" {
			label += " " + detail
		}
		s.toolsStarted = appendLimited(s.toolsStarted, strings.TrimSpace(label), 8)
	case "tool.output":
		line := strings.TrimSpace(event.Content)
		if line != "" {
			s.lastOutputs = appendLimited(s.lastOutputs, line, 5)
		}
	case "tool.completed":
		if event.Err != nil {
			label := event.ToolName
			if label == "" {
				label = "tool"
			}
			s.toolFailures = appendLimited(s.toolFailures, fmt.Sprintf("%s: %v", label, event.Err), 5)
		}
	case "turn.completed":
		s.completion.Status = payloadString(event.Payload, "status")
		s.completion.CompletionReason = payloadString(event.Payload, "completion_reason")
		s.completion.FinishReason = payloadString(event.Payload, "finish_reason")
		s.completion.Resumable = payloadBool(event.Payload, "resumable")
	}
}

func (s EventSummary) Completion() TurnCompletion {
	return s.completion
}

func (s EventSummary) WithContent(content string) string {
	content = strings.TrimSpace(content)
	if s.completion.Status == "incomplete" {
		reason := humanCompletionReason(s.completion.CompletionReason)
		notice := "SelfMind stopped before full completion"
		if reason != "" {
			notice += " (" + reason + ")"
		}
		if s.completion.Resumable {
			notice += "; reply \"continue\" to resume."
		} else {
			notice += "."
		}
		if content == "" {
			return notice
		}
		return content + "\n\n" + notice
	}
	if content != "" {
		return content
	}
	if len(s.toolFailures) > 0 {
		return "SelfMind encountered a tool error before producing a final response. Review the tool events above, then retry or ask me to continue."
	}
	if len(s.phases) > 0 || len(s.toolsStarted) > 0 || len(s.lastOutputs) > 0 {
		return "SelfMind finished this turn without producing a final response. Please retry or ask me to continue."
	}
	return ""
}

func payloadString(payload map[string]interface{}, key string) string {
	if payload == nil {
		return ""
	}
	value, _ := payload[key].(string)
	return strings.TrimSpace(value)
}

func payloadBool(payload map[string]interface{}, key string) bool {
	if payload == nil {
		return false
	}
	value, _ := payload[key].(bool)
	return value
}

func humanCompletionReason(reason string) string {
	switch strings.TrimSpace(reason) {
	case "tool_budget_exhausted":
		return "tool budget exhausted"
	case "output_limit":
		return "model output limit reached"
	case "max_iterations":
		return "iteration limit reached"
	default:
		return strings.ReplaceAll(strings.TrimSpace(reason), "_", " ")
	}
}

func appendLimited(items []string, item string, limit int) []string {
	if item == "" {
		return items
	}
	items = append(items, item)
	if limit > 0 && len(items) > limit {
		return items[len(items)-limit:]
	}
	return items
}

func toolArgsDetail(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return ""
	}
	for _, key := range []string{"command", "path", "query", "pattern", "name"} {
		if v, ok := args[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
