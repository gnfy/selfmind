package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"selfmind/internal/kernel"
)

type batchReadContextKey struct{}

func TestBatchReadIsBoundedReadOnlyAndFallsBackPerItem(t *testing.T) {
	var calls []string
	ctx := context.WithValue(context.Background(), batchReadContextKey{}, "run")
	tool := NewBatchReadTool(func(name string, args map[string]interface{}) (string, error) {
		calls = append(calls, name)
		if ContextFromArgs(args).Value(batchReadContextKey{}) != "run" {
			t.Fatalf("child call lost run context")
		}
		if name == "search_files" {
			return "partial evidence", fmt.Errorf("search failed")
		}
		return "ok", nil
	})
	out, err := tool.ExecuteContext(ctx, map[string]interface{}{
		"candidate_id": "evo-1",
		"_context":     ctx,
		"operations": []interface{}{
			map[string]interface{}{"tool": "read_file", "path": "a.txt"},
			map[string]interface{}{"tool": "search_files", "pattern": "needle", "path": "."},
		},
	})
	if err != nil {
		t.Fatalf("ExecuteContext: %v", err)
	}
	var payload struct {
		Success          bool `json:"success"`
		Failures         int  `json:"failures"`
		FallbackRequired bool `json:"fallback_required"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Success || payload.Failures != 1 || !payload.FallbackRequired {
		t.Fatalf("payload = %#v", payload)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %v", calls)
	}

	_, err = tool.ExecuteContext(ctx, map[string]interface{}{
		"operations": []interface{}{map[string]interface{}{"tool": "terminal", "path": "."}},
	})
	if err == nil {
		t.Fatal("terminal must never be accepted by batch_read")
	}
}

func TestBatchReadChargesAndStopsAtNestedToolBudget(t *testing.T) {
	dispatched := 0
	tool := NewBatchReadTool(func(name string, args map[string]interface{}) (string, error) {
		dispatched++
		return name, nil
	})
	ctx, used := kernel.WithNestedActionToolBudget(context.Background(), 1)
	out, err := tool.ExecuteContext(ctx, map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{"tool": "read_file", "path": "a.txt"},
			map[string]interface{}{"tool": "read_file", "path": "b.txt"},
		},
	})
	if err != nil {
		t.Fatalf("ExecuteContext: %v", err)
	}
	if dispatched != 1 || used() != 1 {
		t.Fatalf("dispatched=%d used=%d, want 1/1", dispatched, used())
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["fallback_required"] != true || int(payload["failures"].(float64)) != 1 {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestBatchReadNestedEventsCarryCanonicalIdentityAndResults(t *testing.T) {
	events := make(chan string, 16)
	ctx := kernel.WithEventChannel(context.Background(), events)
	tool := NewBatchReadTool(func(name string, args map[string]interface{}) (string, error) {
		if name == "search_files" {
			return "partial", fmt.Errorf("search failed")
		}
		return "package main", nil
	})

	_, err := tool.ExecuteContext(ctx, map[string]interface{}{
		"_tool_call_id": "parent-call",
		"operations": []interface{}{
			map[string]interface{}{"tool": "read_file", "path": "main.go"},
			map[string]interface{}{"tool": "search_files", "pattern": "needle", "path": "."},
		},
	})
	if err != nil {
		t.Fatalf("ExecuteContext: %v", err)
	}

	var lifecycle []kernel.AgentEvent
	for len(events) > 0 {
		raw := <-events
		event, ok := kernel.DecodeAgentEvent(raw)
		if ok && (event.Type == "tool.started" || event.Type == "tool.completed") {
			lifecycle = append(lifecycle, event)
		}
	}
	if len(lifecycle) != 4 {
		t.Fatalf("lifecycle events = %d, want 4: %+v", len(lifecycle), lifecycle)
	}
	for i, event := range lifecycle {
		wantCallID := "parent-call:batch:1"
		if i >= 2 {
			wantCallID = "parent-call:batch:2"
		}
		// Events are start/end pairs for each sequential child.
		if i == 1 {
			wantCallID = "parent-call:batch:1"
		}
		if event.ToolCallID != wantCallID || event.ToolName == "" {
			t.Fatalf("event %d identity = tool %q call %q, want call %q", i, event.ToolName, event.ToolCallID, wantCallID)
		}
		if event.Payload["parent_tool_call_id"] != "parent-call" {
			t.Fatalf("event %d parent = %#v", i, event.Payload["parent_tool_call_id"])
		}
	}
	if lifecycle[0].ToolArgs == "" || lifecycle[1].ToolResult != "package main" {
		t.Fatalf("first child fields = start args %q end result %q", lifecycle[0].ToolArgs, lifecycle[1].ToolResult)
	}
	if lifecycle[3].Error != "search failed" || lifecycle[3].ToolResult != "partial" {
		t.Fatalf("failed child completion = %+v", lifecycle[3])
	}
}

func TestBatchReadChildArgsNeverFabricatesNilCallID(t *testing.T) {
	child := batchReadChildArgs(batchReadOperation{Tool: "read_file", Path: "main.go"}, map[string]interface{}{}, 0)
	if callID, exists := child["_tool_call_id"]; exists {
		t.Fatalf("missing parent identity produced child call id %#v", callID)
	}

	child = batchReadChildArgs(batchReadOperation{Tool: "read_file", Path: "main.go"}, map[string]interface{}{"_tool_call_id": " parent "}, 1)
	if got := child["_tool_call_id"]; got != "parent:batch:2" {
		t.Fatalf("derived child call id = %#v, want parent:batch:2", got)
	}
}
