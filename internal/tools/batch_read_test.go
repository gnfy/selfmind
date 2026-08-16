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
