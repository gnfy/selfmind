package router

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/runpool"
)

// runIdleTimeout reads SELFMIND_RUN_IDLE_TIMEOUT (e.g. "5m"). The personal
// daemon defaults to a conservative ten-minute idle ceiling: commands emit
// heartbeats while they make progress, so silence for that long means the run
// has stopped producing evidence. Set 0/off/disabled to opt out.
func runIdleTimeout() time.Duration {
	v := strings.TrimSpace(os.Getenv("SELFMIND_RUN_IDLE_TIMEOUT"))
	if v == "" {
		return 10 * time.Minute
	}
	switch strings.ToLower(v) {
	case "0", "off", "disabled":
		return 0
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	return 10 * time.Minute
}

// PrepareRunWatchdog installs the run watchdog before execution-scope
// callbacks capture the context. This is intentionally owned by the run
// coordinator; withAgentEvents only supplies progress activity and remains a
// fallback for embedders that call the router directly.
func PrepareRunWatchdog(ctx context.Context) (context.Context, func()) {
	if runpool.HasWatchdog(ctx) {
		return ctx, func() {}
	}
	ctx, _, stop := runpool.WithWatchdog(ctx, runIdleTimeout())
	return ctx, stop
}

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

	// Progress watchdog: cancels the run if it goes silent for too long, so a
	// stuck provider/tool frees its worker. Disabled by default (idle<=0).
	activity := func() { runpool.RecordActivity(ctx) }
	stop := func() {}
	if !runpool.HasWatchdog(ctx) {
		ctx, activity, stop = runpool.WithWatchdog(ctx, runIdleTimeout())
	}

	resp, err := run(ctx)
	if err != nil || resp == nil || !resp.IsStreaming {
		stop()
		return resp, err
	}

	orig := resp.Stream
	out := make(chan llm.StreamEvent, 1024)
	resp.Stream = out
	go func() {
		defer close(out)
		defer stop()
		origOpen := true
		for origOpen {
			select {
			case raw := <-eventCh:
				activity()
				out <- agentEventToStream(raw)
			case ev, ok := <-orig:
				if !ok {
					origOpen = false
					continue
				}
				activity()
				out <- ev
			case <-ctx.Done():
				out <- llm.StreamEvent{Err: watchdogError(ctx)}
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

// watchdogError turns a watchdog stall into an actionable message; otherwise it
// surfaces the underlying cancellation.
func watchdogError(ctx context.Context) error {
	if cause := context.Cause(ctx); cause == runpool.ErrStalled {
		return fmt.Errorf("%w: no progress for %s; freed the worker; please retry or refine the request", runpool.ErrStalled, runIdleTimeout())
	}
	return ctx.Err()
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
