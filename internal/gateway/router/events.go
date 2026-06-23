package router

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
)

func (g *Gateway) HandleWithEvents(ctx context.Context, unifiedUID, channel, input string) (*HandleResponse, error) {
	if g == nil || g.agent == nil {
		if g == nil {
			return nil, fmt.Errorf("gateway is not configured")
		}
		return g.Handle(ctx, unifiedUID, channel, input)
	}
	return g.withAgentEvents(ctx, func(ctx context.Context) (*HandleResponse, error) {
		return g.Handle(ctx, unifiedUID, channel, input)
	})
}

func (g *Gateway) RunAgentWithEvents(ctx context.Context, unifiedUID, channel, input string) (*HandleResponse, error) {
	if g == nil {
		return nil, fmt.Errorf("gateway is not configured")
	}
	if g.agent == nil {
		return g.RunAgent(ctx, unifiedUID, channel, input)
	}
	return g.withAgentEvents(ctx, func(ctx context.Context) (*HandleResponse, error) {
		return g.RunAgent(ctx, unifiedUID, channel, input)
	})
}

func (g *Gateway) withAgentEvents(ctx context.Context, run func(context.Context) (*HandleResponse, error)) (*HandleResponse, error) {
	eventCh := make(chan string, 1024)
	ctx = kernel.WithEventChannel(ctx, eventCh)

	resp, err := run(ctx)
	if err != nil || resp == nil || !resp.IsStreaming {
		return resp, err
	}

	orig := resp.Stream
	out := make(chan llm.StreamEvent, 1024)
	resp.Stream = out
	go func() {
		defer close(out)
		origOpen := true
		for origOpen {
			select {
			case raw := <-eventCh:
				out <- agentEventToStream(raw)
			case ev, ok := <-orig:
				if !ok {
					origOpen = false
					continue
				}
				out <- ev
			case <-ctx.Done():
				out <- llm.StreamEvent{Err: ctx.Err()}
				return
			}
		}
		for {
			select {
			case raw := <-eventCh:
				out <- agentEventToStream(raw)
			default:
				return
			}
		}
	}()
	return resp, nil
}

func agentEventToStream(event string) llm.StreamEvent {
	if structured, ok := kernel.DecodeAgentEvent(event); ok {
		payload := structured.Payload
		if payload == nil {
			payload = map[string]interface{}{}
		}
		if len(structured.Plan) > 0 {
			payload["plan"] = structured.Plan
		}
		stream := llm.StreamEvent{
			EventType:       structured.Type,
			Content:         structured.Content,
			ToolName:        structured.ToolName,
			ToolCallID:      structured.ToolCallID,
			ToolArgs:        structured.ToolArgs,
			ToolResult:      structured.ToolResult,
			DurationSeconds: structured.DurationSeconds,
			Payload:         payload,
		}
		if structured.Error != "" {
			stream.Err = fmt.Errorf("%s", structured.Error)
		}
		return stream
	}
	switch {
	case strings.HasPrefix(event, "stream:"):
		return llm.StreamEvent{EventType: "stream", Content: strings.TrimPrefix(event, "stream:")}
	case strings.HasPrefix(event, "tool_start:"):
		rest := strings.TrimPrefix(event, "tool_start:")
		name, args, _ := strings.Cut(rest, ":")
		return llm.StreamEvent{EventType: "tool.started", ToolName: name, ToolArgs: args}
	case strings.HasPrefix(event, "tool_end:"):
		rest := strings.TrimPrefix(event, "tool_end:")
		name, rest, _ := strings.Cut(rest, ":")
		if strings.HasPrefix(rest, "error:") {
			rest = strings.TrimPrefix(rest, "error:")
			durationText, errText, _ := strings.Cut(rest, ":")
			duration, _ := strconv.ParseFloat(durationText, 64)
			return llm.StreamEvent{EventType: "tool.completed", ToolName: name, ToolResult: errText, DurationSeconds: duration, Err: fmt.Errorf("%s", errText)}
		}
		durationText, result, _ := strings.Cut(rest, ":")
		duration, _ := strconv.ParseFloat(durationText, 64)
		return llm.StreamEvent{EventType: "tool.completed", ToolName: name, ToolResult: result, DurationSeconds: duration}
	case strings.HasPrefix(event, "review:"):
		return llm.StreamEvent{EventType: "learning.review", Content: strings.TrimPrefix(event, "review:")}
	default:
		return llm.StreamEvent{EventType: "agent.event", Content: event}
	}
}
