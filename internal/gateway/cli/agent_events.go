package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/httpapi"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
)

// planJSONFromEvent rebuilds the {"explanation", "plan":[{step,status}]} JSON
// that renderPlanCell parses from a plan.updated stream event's payload. The
// payload's "plan" value is a []kernel.PlanItem in the in-process router path
// and a decoded []interface{} in the daemon-client path; json.Marshal handles
// both, so one helper serves every TUI transport. Returns "" if no plan is
// present so the caller renders nothing rather than an empty cell.
func planJSONFromEvent(event llm.StreamEvent) string {
	if event.Payload == nil {
		return ""
	}
	plan, ok := event.Payload["plan"]
	if !ok || plan == nil {
		return ""
	}
	payload := map[string]interface{}{"plan": plan}
	if exp, ok := event.Payload["explanation"].(string); ok && strings.TrimSpace(exp) != "" {
		payload["explanation"] = exp
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return string(data)
}

func (m *uiModel) runAgent(ctx context.Context, input string) tea.Cmd {
	return func() tea.Msg {
		if m.messageProcessor != nil {
			var full strings.Builder
			var usage llm.UsageStats
			ctx = httpapi.WithStreamObserver(ctx, func(event llm.StreamEvent) {
				if event.EventType != "" {
					m.forwardGatewayEvent(event)
					if event.EventType == "stream" {
						full.WriteString(event.Content)
					}
					if event.Usage != nil {
						usage = *event.Usage
					}
					return
				}
				if event.Content != "" {
					full.WriteString(event.Content)
					if m.program != nil {
						m.program.Send(MsgStream{Content: event.Content})
					}
				}
				if event.Usage != nil {
					usage = *event.Usage
				}
			})
			cwd := currentWorkingDir()
			resp, status := m.messageProcessor(ctx, api.MessageRequest{
				TenantID:       m.tenantID,
				Platform:       "cli",
				PlatformUserID: cliPlatformUserID(),
				DisplayName:    cliDisplayName(),
				Channel:        m.channel,
				Content:        input,
				ClientCWD:      cwd,
				Attachments:    imageAttachmentsFromInput(input, cwd),
				ApprovalMode:   m.approvalMode,
			})
			if resp.Usage.InputTokens != 0 || resp.Usage.OutputTokens != 0 {
				usage = resp.Usage
			}
			if resp.Error != "" {
				return MsgAgentDone{Response: full.String(), Usage: usage, Err: fmt.Errorf("%s", resp.Error)}
			}
			if status >= http.StatusBadRequest {
				return MsgAgentDone{Response: full.String(), Usage: usage, Err: fmt.Errorf("gateway returned HTTP %d", status)}
			}
			content := resp.Content
			if strings.TrimSpace(content) == "" {
				content = full.String()
			}
			return MsgAgentDone{Response: content, Usage: usage}
		}

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

// requestDaemonStop asks the gateway to cancel the person's active run via the
// /stop control command. Since G0-a, run lifetime is daemon-owned and the run
// ctx is detached from the endpoint connection, so cancelling the local ctx
// (m.cancelFn) only detaches this watcher — both the in-process gateway
// (ProcessMessage detaches internally) and the daemon client (the aborted HTTP
// request only detaches) need this explicit registry-backed stop. Returns nil
// on the legacy direct-agent path, where the local ctx still owns the run.
func (m *uiModel) requestDaemonStop() tea.Cmd {
	if m.messageProcessor == nil {
		return nil
	}
	processor := m.messageProcessor
	req := m.controlMessageRequest("/stop")
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = processor(ctx, req)
		return nil
	}
}

func cliPlatformUserID() string {
	for _, key := range []string{"SELFMIND_CLI_USER_ID", "SELF_CLI_USER_ID", "USER", "USERNAME"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return "local"
}

func cliDisplayName() string {
	if value := strings.TrimSpace(os.Getenv("SELFMIND_CLI_DISPLAY_NAME")); value != "" {
		return value
	}
	return cliPlatformUserID()
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
	case "agent.thinking", "agent.step":
		if event.Content != "" {
			m.program.Send(MsgAgentActivity{Content: displayActivityEvent(event.EventType, event.Content)})
		}
	case "tool.started":
		// update_plan renders as a plan checklist driven by the plan.updated
		// event (which carries the full structured plan); its raw tool.* events
		// would only add a redundant summary cell, so skip them here.
		if event.ToolName == "update_plan" {
			return
		}
		m.program.Send(MsgToolStart{ToolName: event.ToolName, ToolCallID: event.ToolCallID, Args: event.ToolArgs})
	case "tool.completed":
		if event.ToolName == "update_plan" {
			return
		}
		m.program.Send(MsgToolDone{
			ToolName:   event.ToolName,
			ToolCallID: event.ToolCallID,
			Result:     event.ToolResult,
			Err:        event.Err,
			Duration:   event.DurationSeconds,
		})
	case "plan.updated":
		// Render the live plan as a Codex-style checklist. The event payload holds
		// the full plan (steps + explanation); rebuild the JSON renderPlanCell
		// parses and commit it as an update_plan tool cell. This is the only path
		// that shows the [x]/[>]/[ ] step list in client-mode TUIs (the default).
		if planJSON := planJSONFromEvent(event); planJSON != "" {
			m.program.Send(MsgToolStart{ToolName: "update_plan"})
			m.program.Send(MsgToolDone{ToolName: "update_plan", Result: planJSON})
		}
	case "tool.output":
		if event.Content != "" {
			m.program.Send(MsgToolOutput{ToolName: event.ToolName, Content: event.Content})
		}
	case "tool.heartbeat":
		m.program.Send(MsgToolHeartbeat{ToolName: event.ToolName, ToolCallID: event.ToolCallID, Content: heartbeatStatus(event)})
	case "learning.review":
		if event.Content != "" {
			m.program.Send(MsgLearningEvent{Content: event.Content})
		}
	case "approval.requested":
		id := ""
		if event.Payload != nil {
			if v, ok := event.Payload["approval_id"].(string); ok {
				id = v
			}
		}
		if id != "" {
			m.program.Send(MsgApprovalRequest{ID: id, Tool: event.ToolName, Reason: event.Content})
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
	case "agent.thinking", "agent.step":
		if event.Content != "" {
			m.program.Send(MsgAgentActivity{Content: displayActivityEvent(event.Type, event.Content)})
		}
	case "tool.started":
		m.program.Send(MsgToolStart{ToolName: event.ToolName, ToolCallID: event.ToolCallID, Args: event.ToolArgs})
	case "tool.completed":
		var err error
		if event.Error != "" {
			err = fmt.Errorf("%s", event.Error)
		}
		m.program.Send(MsgToolDone{
			ToolName:   event.ToolName,
			ToolCallID: event.ToolCallID,
			Result:     event.ToolResult,
			Err:        err,
			Duration:   event.DurationSeconds,
		})
	case "tool.output":
		if event.Content != "" {
			m.program.Send(MsgToolOutput{ToolName: event.ToolName, Content: event.Content})
		}
	case "tool.heartbeat":
		m.program.Send(MsgToolHeartbeat{ToolName: event.ToolName, ToolCallID: event.ToolCallID, Content: heartbeatStatus(llm.StreamEvent{
			EventType:  event.Type,
			ToolName:   event.ToolName,
			ToolCallID: event.ToolCallID,
			Payload:    event.Payload,
		})})
	case "plan.updated":
		m.program.Send(MsgLearningEvent{Content: "Plan updated."})
	case "run.outcome":
		m.program.Send(MsgLearningEvent{Content: "Outcome recorded."})
	case "learning.review":
		m.program.Send(MsgLearningEvent{Content: event.Content})
	}
}

func displayActivityEvent(eventType, content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	return content
}

func heartbeatStatus(event llm.StreamEvent) string {
	name := event.ToolName
	if name == "" {
		name = "tool"
	}
	detail := strings.TrimSpace(event.Content)
	if event.Payload != nil {
		if detail == "" {
			if status, ok := event.Payload["status"].(string); ok {
				detail = strings.TrimSpace(status)
			}
		}
	}
	if detail != "" {
		return detail
	}
	return name + " running"
}
