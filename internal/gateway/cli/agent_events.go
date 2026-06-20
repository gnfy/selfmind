package cli

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
)

func (m *uiModel) runAgent(ctx context.Context, input string) tea.Cmd {
	return func() tea.Msg {
		if m.gateway != nil {
			resp, err := m.gateway.HandleWithEvents(ctx, m.tenantID, m.channel, input)
			if err != nil {
				return MsgAgentDone{Err: err}
			}
			if resp == nil {
				return MsgAgentDone{Err: fmt.Errorf("gateway returned no response")}
			}

			if !resp.IsStreaming {
				return MsgAgentDone{Response: resp.Content, Usage: resp.Usage, Err: nil}
			}

			var full strings.Builder
			var usage llm.UsageStats
			sawStream := false
			for event := range resp.Stream {
				if event.Err != nil && event.EventType == "" {
					return MsgAgentDone{Response: full.String(), Usage: usage, Err: event.Err}
				}
				if event.EventType != "" {
					m.forwardGatewayEvent(event)
					if event.EventType == "stream" {
						sawStream = true
						full.WriteString(event.Content)
					}
					if event.Usage != nil {
						usage = *event.Usage
					}
					continue
				}
				if event.Content != "" && !sawStream {
					full.WriteString(event.Content)
					if m.program != nil {
						m.program.Send(MsgStream{Content: event.Content})
					}
				}
				if event.Usage != nil {
					usage = *event.Usage
				}
			}
			return MsgAgentDone{Response: full.String(), Usage: usage}
		}

		go m.pumpAgentEvents()
		resp, usage, err := m.agent.RunConversation(ctx, m.tenantID, m.channel, input)
		return MsgAgentDone{Response: resp, Usage: usage, Err: err}
	}
}

func (m *uiModel) forwardGatewayEvent(event llm.StreamEvent) {
	if m.program == nil {
		return
	}
	switch event.EventType {
	case "stream":
		if event.Content != "" {
			m.program.Send(MsgStream{Content: event.Content})
		}
	case "tool.started":
		m.program.Send(MsgToolStart{ToolName: event.ToolName, Args: event.ToolArgs})
	case "tool.completed":
		m.program.Send(MsgToolDone{
			ToolName: event.ToolName,
			Result:   event.ToolResult,
			Err:      event.Err,
			Duration: event.DurationSeconds,
		})
	case "tool.output":
		if event.Content != "" {
			m.program.Send(MsgToolOutput{ToolName: event.ToolName, Content: event.Content})
		}
	case "tool.heartbeat":
		m.program.Send(MsgToolHeartbeat{ToolName: event.ToolName, Content: heartbeatStatus(event)})
	case "learning.review":
		if event.Content != "" {
			m.program.Send(MsgLearningEvent{Content: event.Content})
		}
	}
}

func (m *uiModel) pumpAgentEvents() {
	if m.agent == nil || m.agent.EventChannel == nil {
		return
	}
	for {
		select {
		case event, ok := <-m.agent.EventChannel:
			if !ok {
				return
			}
			if structured, ok := kernel.DecodeAgentEvent(event); ok {
				m.handleStructuredAgentEvent(structured)
				continue
			}
			switch {
			case strings.HasPrefix(event, "stream:"):
				content := strings.TrimPrefix(event, "stream:")
				if m.program != nil {
					m.program.Send(MsgStream{Content: content})
				}
			case strings.HasPrefix(event, "tool_start:"):
				parts := strings.SplitN(event[11:], ":", 2)
				name := parts[0]
				args := ""
				if len(parts) > 1 {
					args = parts[1]
				}
				if m.program != nil {
					m.program.Send(MsgToolStart{ToolName: name, Args: args})
				}
			case strings.HasPrefix(event, "tool_end:"):
				rest := strings.TrimPrefix(event, "tool_end:")
				parts := strings.SplitN(rest, ":", 3)
				name := parts[0]
				durationStr := "0"
				result := ""
				var err error
				if len(parts) >= 2 {
					if parts[1] == "error" {
						errParts := strings.SplitN(parts[2], ":", 2)
						durationStr = errParts[0]
						err = fmt.Errorf("%s", errParts[1])
					} else {
						durationStr = parts[1]
						result = parts[2]
					}
				}
				var duration float64
				fmt.Sscanf(durationStr, "%f", &duration)
				if m.program != nil {
					m.program.Send(MsgToolDone{ToolName: name, Result: result, Err: err, Duration: duration})
				}
			case strings.HasPrefix(event, "review:"):
				if m.program != nil {
					m.program.Send(MsgLearningEvent{Content: strings.TrimPrefix(event, "review:")})
				}
			}
		}
	}
}

func (m *uiModel) handleStructuredAgentEvent(event kernel.AgentEvent) {
	if m.program == nil {
		return
	}
	switch event.Type {
	case "stream":
		m.program.Send(MsgStream{Content: event.Content})
	case "tool.started":
		m.program.Send(MsgToolStart{ToolName: event.ToolName, Args: event.ToolArgs})
	case "tool.completed":
		var err error
		if event.Error != "" {
			err = fmt.Errorf("%s", event.Error)
		}
		m.program.Send(MsgToolDone{
			ToolName: event.ToolName,
			Result:   event.ToolResult,
			Err:      err,
			Duration: event.DurationSeconds,
		})
	case "tool.output":
		if event.Content != "" {
			m.program.Send(MsgToolOutput{ToolName: event.ToolName, Content: event.Content})
		}
	case "tool.heartbeat":
		m.program.Send(MsgToolHeartbeat{ToolName: event.ToolName, Content: heartbeatStatus(llm.StreamEvent{
			EventType: event.Type,
			ToolName:  event.ToolName,
			Payload:   event.Payload,
		})})
	case "plan.updated":
		m.program.Send(MsgLearningEvent{Content: "Plan updated."})
	case "run.outcome":
		m.program.Send(MsgLearningEvent{Content: "Outcome recorded."})
	case "learning.review":
		m.program.Send(MsgLearningEvent{Content: event.Content})
	}
}

func heartbeatStatus(event llm.StreamEvent) string {
	name := event.ToolName
	if name == "" {
		name = "tool"
	}
	if event.Payload != nil {
		if elapsed, ok := event.Payload["elapsed_seconds"].(float64); ok && elapsed > 0 {
			return fmt.Sprintf("%s running %.1fs", name, elapsed)
		}
	}
	return name + " running"
}
