package cli

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

func (m *uiModel) runAgent(ctx context.Context, input string) tea.Cmd {
	return func() tea.Msg {
		go m.pumpAgentEvents()

		if m.gateway != nil {
			resp, err := m.gateway.Handle(ctx, m.tenantID, m.channel, input)
			if err != nil {
				return MsgAgentDone{Err: err}
			}

			if !resp.IsStreaming {
				return MsgAgentDone{Response: resp.Content, Usage: resp.Usage, Err: nil}
			}

			for event := range resp.Stream {
				if event.Err != nil {
					return MsgAgentDone{Err: event.Err}
				}
				if event.Usage != nil {
					return MsgAgentDone{Usage: *event.Usage}
				}
			}
			return nil
		}

		resp, usage, err := m.agent.RunConversation(ctx, m.tenantID, m.channel, input)
		return MsgAgentDone{Response: resp, Usage: usage, Err: err}
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
