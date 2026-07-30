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
				// Session workspace override from /workspace this session; the
				// server honors an explicit WorkspaceID before deriving one from
				// ClientCWD, so the turn runs in the selected workspace. Empty
				// keeps the cwd-derived default.
				WorkspaceID: m.workspaceOverrideID,
			})
			if resp.Usage.InputTokens != 0 || resp.Usage.OutputTokens != 0 {
				usage = resp.Usage
			}
			if resp.Error != "" {
				return MsgAgentDone{Response: full.String(), Usage: usage, Err: fmt.Errorf("%s", resp.Error), Input: input, Turn: resp.Turn}
			}
			if status >= http.StatusBadRequest {
				return MsgAgentDone{Response: full.String(), Usage: usage, Err: fmt.Errorf("gateway returned HTTP %d", status), Input: input, Turn: resp.Turn}
			}
			content := resp.Content
			if strings.TrimSpace(content) == "" {
				content = full.String()
			}
			return MsgAgentDone{Response: content, Usage: usage, Input: input, Turn: resp.Turn}
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
	m.forwardGatewayEventFrom(event, eventSourceTurn)
}

func (m *uiModel) forwardGatewayEventFrom(event llm.StreamEvent, source eventSource) {
	if m.program == nil {
		return
	}
	ref := eventRefFromStream(event, source)
	switch event.EventType {
	case "stream":
		if event.Content != "" {
			m.program.Send(MsgStream{Content: event.Content, Event: ref})
		}
	case "agent.thinking", "agent.step":
		if event.Content != "" {
			m.program.Send(MsgAgentActivity{Content: displayActivityEvent(event.EventType, event.Content), Event: ref})
		}
	case "tool.started":
		// update_plan renders as a plan checklist driven by the plan.updated
		// event (which carries the full structured plan); its raw tool.* events
		// would only add a redundant summary cell, so skip them here.
		if isHiddenLifecycleTool(event.ToolName) {
			return
		}
		m.program.Send(MsgToolStart{ToolName: event.ToolName, ToolCallID: event.ToolCallID, Args: event.ToolArgs, Event: ref})
	case "tool.completed":
		if isHiddenLifecycleTool(event.ToolName) {
			return
		}
		m.program.Send(MsgToolDone{
			ToolName:   event.ToolName,
			ToolCallID: event.ToolCallID,
			Result:     event.ToolResult,
			Err:        event.Err,
			Duration:   event.DurationSeconds,
			Event:      ref,
		})
	case "plan.updated":
		// A plan is mutable run state, not immutable terminal history. Replace the
		// live snapshot above the composer so progress updates in place.
		if planJSON := planJSONFromEvent(event); planJSON != "" {
			m.program.Send(MsgPlanUpdated{Content: planJSON, Event: ref})
		}
	case "tool.output":
		if event.Content != "" {
			m.program.Send(MsgToolOutput{ToolName: event.ToolName, ToolCallID: event.ToolCallID, Content: event.Content, Event: ref})
		}
	case "tool.heartbeat":
		m.program.Send(MsgToolHeartbeat{ToolName: event.ToolName, ToolCallID: event.ToolCallID, Content: heartbeatStatus(event), Event: ref})
	case "learning.review":
		if event.Content != "" {
			m.program.Send(MsgLearningEvent{Content: event.Content, Event: ref})
		}
	case "watch.completed":
		watchID, status, taskStatus := "", "", ""
		if event.Payload != nil {
			watchID, _ = event.Payload["watch_id"].(string)
			status, _ = event.Payload["status"].(string)
			taskStatus, _ = event.Payload["task_status"].(string)
		}
		if strings.TrimSpace(watchID) != "" {
			m.program.Send(MsgWatcherCompleted{
				WatchID:    strings.TrimSpace(watchID),
				Status:     strings.TrimSpace(status),
				TaskStatus: strings.TrimSpace(taskStatus),
				Event:      ref,
			})
		}
	case "token.updated":
		// Live cumulative usage for the active run. The daemon-client path
		// carries it in Usage (client.eventToStream); the in-process gateway
		// path carries the raw payload (agentEventToStream), so read both.
		if run := runTokensFromEvent(event); run > 0 {
			m.program.Send(MsgTokens{Run: run, Event: ref})
		}
	case "approval.requested":
		payloadString := func(key string) string {
			if event.Payload == nil {
				return ""
			}
			v, _ := event.Payload[key].(string)
			return v
		}
		id := payloadString("approval_id")
		if id != "" {
			m.program.Send(MsgApprovalRequest{
				ID:     id,
				Tool:   event.ToolName,
				Target: payloadString("target"),
				Reason: event.Content,
				// Decision context; absent from an older daemon's event, in which
				// case the panel renders exactly what it did before.
				Environment:   payloadString("environment"),
				Cwd:           payloadString("cwd"),
				ChangeSummary: payloadString("change_summary"),
				GrantClass:    payloadString("grant_class"),
				TriageState:   payloadString("triage_state"),
				Rationale:     payloadString("triage_rationale"),
				Risk:          payloadString("triage_risk"),
				// The daemon's own answer set for this ask. Absent (older daemon)
				// leaves it nil and the panel falls back to its built-in options.
				Options: approvalOptionsFromPayload(event.Payload),
			})
		}
	case "clarify.requested":
		if strings.TrimSpace(event.Content) == "" {
			return
		}
		id := ""
		if event.Payload != nil {
			if v, ok := event.Payload["clarify_id"].(string); ok {
				id = v
			}
		}
		m.program.Send(MsgClarifyRequest{ID: id, Question: event.Content, Choices: clarifyChoicesFromPayload(event.Payload)})
	}
}

func isHiddenLifecycleTool(name string) bool {
	switch strings.TrimSpace(name) {
	case "update_plan", "finish_run":
		return true
	default:
		return false
	}
}

func clarifyChoicesFromPayload(payload map[string]interface{}) []string {
	if payload == nil || payload["choices"] == nil {
		return nil
	}
	switch raw := payload["choices"].(type) {
	case []string:
		return raw
	case []interface{}:
		choices := make([]string, 0, len(raw))
		for _, item := range raw {
			if s := strings.TrimSpace(fmt.Sprint(item)); s != "" {
				choices = append(choices, s)
			}
		}
		return choices
	default:
		return nil
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
				if isHiddenLifecycleTool(name) {
					continue
				}
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
				if isHiddenLifecycleTool(name) {
					continue
				}
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
		if isHiddenLifecycleTool(event.ToolName) {
			return
		}
		m.program.Send(MsgToolStart{ToolName: event.ToolName, ToolCallID: event.ToolCallID, Args: event.ToolArgs})
	case "tool.completed":
		if isHiddenLifecycleTool(event.ToolName) {
			return
		}
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
			m.program.Send(MsgToolOutput{ToolName: event.ToolName, ToolCallID: event.ToolCallID, Content: event.Content})
		}
	case "tool.heartbeat":
		m.program.Send(MsgToolHeartbeat{ToolName: event.ToolName, ToolCallID: event.ToolCallID, Content: heartbeatStatus(llm.StreamEvent{
			EventType:  event.Type,
			ToolName:   event.ToolName,
			ToolCallID: event.ToolCallID,
			Payload:    event.Payload,
		})})
	case "token.updated":
		if run := payloadTokenCount(event.Payload["input_tokens"]) + payloadTokenCount(event.Payload["output_tokens"]); run > 0 {
			m.program.Send(MsgTokens{Run: run})
		}
	case "plan.updated":
		streamEvent := llm.StreamEvent{EventType: event.Type, Payload: event.Payload}
		if planJSON := planJSONFromEvent(streamEvent); planJSON != "" {
			m.program.Send(MsgPlanUpdated{Content: planJSON})
		}
	case "run.outcome":
		m.program.Send(MsgLearningEvent{Content: "Outcome recorded."})
	case "learning.review":
		m.program.Send(MsgLearningEvent{Content: event.Content})
	}
}

// runTokensFromEvent extracts the cumulative run token count from a
// token.updated stream event: prefer the typed Usage snapshot, fall back to
// the raw payload fields (input_tokens/output_tokens survive a JSON round
// trip as float64, or stay int on pure in-process delivery).
func runTokensFromEvent(event llm.StreamEvent) int {
	if event.Usage != nil {
		return event.Usage.InputTokens + event.Usage.OutputTokens
	}
	if event.Payload == nil {
		return 0
	}
	return payloadTokenCount(event.Payload["input_tokens"]) + payloadTokenCount(event.Payload["output_tokens"])
}

func payloadTokenCount(v interface{}) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		f, _ := n.Float64()
		return int(f)
	}
	return 0
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
