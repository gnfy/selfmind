package tools

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/kernel"
)

func TestTerminalToolEmitsStreamingOutput(t *testing.T) {
	ch := make(chan string, 10)
	ctx := kernel.WithEventChannel(context.Background(), ch)

	out, err := NewExecuteCommandTool().Execute(map[string]interface{}{
		"command":  "echo hello",
		"timeout":  5,
		"_context": ctx,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("output = %q, want hello", out)
	}
	if !eventSeen(ch, "tool.output") {
		t.Fatalf("tool.output event was not emitted")
	}
}

func TestTerminalToolEmitsHeartbeatForLongCommand(t *testing.T) {
	ch := make(chan string, 20)
	ctx := kernel.WithEventChannel(context.Background(), ch)

	_, err := NewExecuteCommandTool().Execute(map[string]interface{}{
		"command":  "sleep 2",
		"timeout":  5,
		"_context": ctx,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !eventSeen(ch, "tool.heartbeat") {
		t.Fatalf("tool.heartbeat event was not emitted")
	}
}

func eventSeen(ch <-chan string, eventType string) bool {
	for {
		select {
		case raw := <-ch:
			event, ok := kernel.DecodeAgentEvent(raw)
			if ok && event.Type == eventType {
				return true
			}
		default:
			return false
		}
	}
}
