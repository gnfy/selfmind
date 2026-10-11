package kernel

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
)

func TestNativeInputBaselineIsScopedAndConservative(t *testing.T) {
	engine := NewContextEngine(1000, 100)
	b := &requestTokenBaseline{}
	ctx := context.WithValue(context.Background(), requestTokenBaselineKey{}, b)
	base := []llm.Message{{Role: "system", Content: "policy"}, {Role: "user", Content: "goal"}}
	local := engine.countMessages(base)
	b.configure("provider/model/protocol/schema-a")
	b.submitted(base, local)
	b.observed("succeeded", llm.UsageStats{InputTokens: 600, CacheReadInputTokens: 500, OutputTokens: 80})
	if got, measured := engine.requestTokens(ctx, base, nil); got != 600 || !measured {
		t.Fatalf("native baseline=%d measured=%t; cache/output must not be counted twice", got, measured)
	}
	appended := append(cloneLoopMessages(base), llm.Message{Role: "assistant", ReasoningContent: strings.Repeat("reason ", 20)})
	want := 600 + engine.countMessages(appended) - local
	if got, measured := engine.requestTokens(ctx, appended, nil); got != want || !measured {
		t.Fatalf("delta=%d want=%d measured=%t", got, want, measured)
	}
	changed := cloneLoopMessages(appended)
	changed[0].Content = "replacement policy"
	if _, measured := engine.requestTokens(ctx, changed, nil); measured {
		t.Fatal("history edits reused incompatible native usage")
	}
	for _, key := range []string{"provider/model/protocol/schema-b", "other/model/protocol/schema-a", "provider/new-model/protocol/schema-a", "provider/model/other-protocol/schema-a", ""} {
		b.configure(key)
		if _, measured := engine.requestTokens(ctx, base, nil); measured {
			t.Fatal("route/schema switch retained old usage")
		}
	}
	b.configure("provider/model/protocol/schema-a")
	b.submitted(base, local)
	b.observed("succeeded", llm.UsageStats{InputTokens: 1})
	if got, _ := engine.requestTokens(ctx, base, nil); got < local {
		t.Fatal("native calibration reduced the local floor")
	}
	b.observed("succeeded", llm.UsageStats{})
	if _, measured := engine.requestTokens(ctx, base, nil); measured {
		t.Fatal("missing usage was treated as real zero")
	}
}

func TestRequestBudgetUsesNativeInputBeforeDispatch(t *testing.T) {
	engine := NewContextEngine(200, 20)
	messages := []llm.Message{{Role: "system", Content: "mandatory policy"}, {Role: "user", Content: "do not publish"}}
	b := &requestTokenBaseline{}
	b.configure("provider/model/protocol/schema")
	b.submitted(messages, engine.countMessages(messages))
	b.observed("succeeded", llm.UsageStats{InputTokens: 250})
	ctx := context.WithValue(context.Background(), requestTokenBaselineKey{}, b)
	if _, err := engine.PrepareRequest(ctx, messages, nil); err == nil {
		t.Fatal("local undercount dispatched oversized required context despite native evidence")
	}
}

func TestDirectWorkSelectionPlanRequiresCommittedResume(t *testing.T) {
	call := llm.ToolCall{Function: "work_select"}
	for _, tc := range []struct {
		raw      string
		accepted bool
	}{
		{`{"status":"committed","commit_mode":"direct","action":"resume","plan":[{"step":"check","status":"pending"}]}`, true},
		{`{"status":"committed","commit_mode":"direct","action":"observe","plan":[{"step":"check","status":"pending"}]}`, false},
		{`{"status":"proposed","action":"resume","plan":[{"step":"check","status":"pending"}]}`, false},
		{`{"status":"committed","commit_mode":"transfer","action":"resume","plan":[{"step":"check","status":"pending"}]}`, false},
		{`{"status":"committed","commit_mode":"direct","action":"resume","plan":null}`, false},
	} {
		steps, ok := unresolvedPlanStepsFromResult(call, toolExecutionResult{rawResult: tc.raw})
		if ok != tc.accepted || ok && (len(steps) != 1 || steps[0] != "check") {
			t.Fatalf("accepted=%t steps=%v for %s", ok, steps, tc.raw)
		}
	}
}
