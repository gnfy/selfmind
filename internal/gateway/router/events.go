package router

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"selfmind/internal/kernel/llm"
)

func (g *Gateway) HandleWithEvents(ctx context.Context, unifiedUID, channel, input string) (*HandleResponse, error) {
	if g == nil || g.agent == nil {
		return g.Handle(ctx, unifiedUID, channel, input)
	}
	eventCh := make(chan string, 100)
	oldCh := g.agent.EventChannel
	g.agent.EventChannel = eventCh

	resp, err := g.Handle(ctx, unifiedUID, channel, input)
	if err != nil || resp == nil || !resp.IsStreaming {
		g.agent.EventChannel = oldCh
		return resp, err
	}

	orig := resp.Stream
	out := make(chan llm.StreamEvent, 50)
	resp.Stream = out
	go func() {
		defer close(out)
		defer func() {
			g.agent.EventChannel = oldCh
		}()
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
