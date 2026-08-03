package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPatchMissIsBoundedAndDoesNotUseSubsequenceGuessing(t *testing.T) {
	lines := make([]string, 4000)
	for i := range lines {
		lines[i] = "actual line"
	}
	start := time.Now()
	got, count, strategy, err := fuzzyFindAndReplace(context.Background(),
		strings.Join(lines, "\n"), "stale heading\nstale value", "replacement")
	if err == nil || count != 0 || strategy != "" {
		t.Fatalf("miss = count %d strategy %q err %v", count, strategy, err)
	}
	if got != strings.Join(lines, "\n") {
		t.Fatal("a missed hunk changed the input")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("bounded miss took %s", elapsed)
	}
}

func TestPatchRejectsAmbiguousNormalizedMatch(t *testing.T) {
	text := "section\n  value = one\nsection\n\tvalue   = one"
	_, count, _, err := fuzzyFindAndReplace(context.Background(), text,
		"section\nvalue = one", "section\nvalue = two")
	if err == nil || count != 0 || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous match = count %d err %v", count, err)
	}
}

func TestPatchHonorsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, count, _, err := fuzzyFindAndReplace(ctx, "before", "before", "after")
	if !errors.Is(err, context.Canceled) || count != 0 {
		t.Fatalf("cancelled match = count %d err %v", count, err)
	}
}

type contextProbeTool struct {
	BaseTool
	want context.Context
}

func (t *contextProbeTool) ExecuteContext(ctx context.Context, _ map[string]interface{}) (string, error) {
	if ctx != t.want {
		return "", errors.New("dispatcher lost tool context")
	}
	return "ok", nil
}

func TestDispatcherPassesRunContextToContextTool(t *testing.T) {
	ctx := context.WithValue(context.Background(), struct{}{}, "run")
	tool := &contextProbeTool{
		BaseTool: BaseTool{name: "context_probe", schema: ToolSchema{Type: "object"}},
		want:     ctx,
	}
	d := NewDispatcher()
	d.RegisterTool(tool)
	got, err := d.Dispatch(tool.Name(), map[string]interface{}{"_context": ctx})
	if err != nil || got != "ok" {
		t.Fatalf("dispatch = %q, %v", got, err)
	}
}
