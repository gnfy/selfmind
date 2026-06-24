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
	}
}

func (s EventSummary) WithContent(content string) string {
	content = strings.TrimSpace(content)
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
