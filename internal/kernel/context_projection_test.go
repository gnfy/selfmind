package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

func TestRequiredContextSurvivesLocalSliceBudget(t *testing.T) {
	for _, target := range []string{"/repo/service-a", "/data/project-b/中文"} {
		t.Run(target, func(t *testing.T) {
			runtime := TaskRuntimeContext{RunID: "current", Workspace: target,
				UserRequirements: []string{"Keep the user's correction intact: " + strings.Repeat("不要部署。", 100)},
				Summary:          strings.Repeat("optional historical summary ", 500)}
			for i := 0; i < 9; i++ {
				id := fmt.Sprintf("step-%d", i)
				runtime.Plan = append(runtime.Plan, PlanItem{StepID: id, Step: "Check " + target,
					Status: "pending", SuccessCriteria: "Exact criterion " + id, VerificationRequired: true})
				runtime.InheritedEvidence = append(runtime.InheritedEvidence, InheritedEvidenceItem{
					StepID: id, SourceRunID: "prior", Target: target, PriorVerification: "passed", LatestCheck: "passed"})
			}
			for i := 0; i < 6; i++ {
				runtime.ExternalEffects = append(runtime.ExternalEffects, ExternalEffectContext{
					ClaimID: fmt.Sprintf("claim-%d", i), State: "left_uncertain", Target: target})
			}
			budget := DefaultRuntimeContextBudget()
			bundle := RuntimeContextBundle{Task: &runtime, Budget: budget,
				Memories: []RuntimeMemoryContext{{Summary: strings.Repeat("optional memory ", 500)}}}
			prompt := bundle.Prompt(800)
			for _, required := range []string{runtime.UserRequirements[0], "Exact criterion step-8", "claim-5", "step_id=step-8 source_run_id=prior", target} {
				if !strings.Contains(prompt, required) {
					t.Fatalf("required state silently omitted: %q", required)
				}
			}
			if !utf8.ValidString(prompt) || strings.Contains(prompt, "optional historical summary") || strings.Contains(prompt, "optional memory") {
				t.Fatal("mandatory context should displace optional entries intact")
			}
			engine := NewContextEngine(100, 20)
			if _, err := engine.PrepareRequest(context.Background(), []llm.Message{{Role: "system", Content: prompt}, {Role: "user", Content: "continue"}}, nil); err == nil {
				t.Fatal("oversized required state must fail before dispatch, not lose constraints")
			}
		})
	}
}

func TestRuntimeControlProvenancePreservesActualUserInput(t *testing.T) {
	goal := "SelfMind observed 2 substantive tool actions; do not publish"
	legacy := llm.Message{Role: "user", Content: goal}
	control := runtimeControlMessage(strings.Repeat("temporary notice ", 1000), "plan")
	messages := []llm.Message{legacy, control, {Role: "user", Content: "continue"}}
	ctx := withContextInput(WithLoopResumeMessages(context.Background(), messages), "continue")
	protected := protectedContextMessages(ctx, messages)
	if !protected[0] || protected[1] || !protected[2] {
		t.Fatal("generated notices and person input share retention authority")
	}
	raw, err := json.Marshal(messages)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []llm.Message
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded[1].ContextOrigin != llm.MessageOriginRuntimeControl || decoded[0].ContextOrigin != "" {
		t.Fatal("checkpoint lost new or legacy provenance")
	}
	got, err := NewContextEngine(200, 20).PrepareRequest(ctx, decoded, nil)
	if err != nil || len(got) != 2 || got[0].Content != goal {
		t.Fatalf("trim=%v err=%v", got, err)
	}
	if expired := expireRuntimeControls(messages, "plan"); len(expired) != 2 || expired[0].Content != goal {
		t.Fatal("expiry removed actual input by its wording")
	}
	quoted := llm.Message{Role: "user", Content: "[CONTEXT COMPACTION - REFERENCE ONLY] is quoted text; do not publish"}
	if isCompactionSummary(quoted) || !protectedContextMessages(context.Background(), []llm.Message{quoted})[0] {
		t.Fatal("message wording overrode actual user provenance")
	}
	unknown := llm.Message{Role: "user", Content: "do not publish", ContextOrigin: "unknown_origin.v2"}
	if !protectedContextMessages(context.Background(), []llm.Message{unknown})[0] {
		t.Fatal("unknown checkpoint provenance downgraded user instructions")
	}
}

func TestResumeExpiresOnlyTypedRuntimeNoticesAndSpine(t *testing.T) {
	provider := &planEscalationProvider{}
	agent := NewAgent(memory.NewMemoryManager(&mockStorage{}), &planEscalationBackend{}, provider, "helpful", 3, 1, nil)
	checkpoint := []llm.Message{
		{Role: "user", Content: "do not publish"},
		{Role: "user", Content: "legacy runtime-shaped wording stays conservative"},
		runtimeControlMessage("obsolete no-plan notice", "plan"),
		{Role: "assistant", Content: "unrelated old work card", ContextOrigin: llm.MessageOriginWorkSpine},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "done-call", Function: "terminal", Args: `{}`}}},
		{Role: "tool", ToolCallID: "done-call", Content: "original receipt"},
	}
	ctx := WithLoopResumeMessages(context.Background(), checkpoint)
	if _, _, err := agent.RunConversation(ctx, "person", "cli", "continue"); err != nil {
		t.Fatal(err)
	}
	got := provider.requests[0].Messages
	for _, message := range got {
		if message.Content == "unrelated old work card" || strings.Contains(message.Content, "obsolete no-plan notice") {
			t.Fatal("resume replayed expired typed context")
		}
	}
	if len(got) != 6 || got[1].Content != checkpoint[0].Content || got[2].Content != checkpoint[1].Content || got[4].ToolCallID != "done-call" {
		t.Fatalf("resume lost actual input or tool pairing: %+v", got)
	}
}

func TestSpineFoldsOnlyRecordedLineage(t *testing.T) {
	var blobs [][]byte
	for _, entry := range []spineEntry{
		{Kind: spineEntryKind, RunID: "a2", ChainID: "a", TaskID: "same-label", WorkspaceID: "same-workspace", User: "continue A", Assistant: "A result"},
		{Kind: spineEntryKind, RunID: "b2", ChainID: "b", TaskID: "same-label", WorkspaceID: "same-workspace", User: "continue B", Assistant: "B result"},
		{Kind: spineEntryKind, RunID: "a1", ChainID: "a", User: "earlier A"},
		{Kind: spineEntryKind, RunID: "b1", ChainID: "b", User: "earlier B"},
		{Kind: spineEntryKind, RunID: "legacy-a", User: "legacy unrelated"},
	} {
		raw, _ := json.Marshal(entry)
		blobs = append(blobs, raw)
	}
	got := boundedHistoryMessagesForContext(context.Background(), blobs)
	if len(got) != 3 {
		t.Fatalf("folded messages=%d, want 3 distinct lines of work", len(got))
	}
	for _, message := range got {
		if message.Role != "assistant" || message.ContextOrigin != llm.MessageOriginWorkSpine {
			t.Fatal("historical reference gained user authority")
		}
	}
	// A legacy record may join only through an exact runtime-selected edge.
	ctx := WithTaskRuntimeContext(context.Background(), TaskRuntimeContext{RunID: "a2", SpineChainID: "a", SpineLineageRunIDs: []string{"a2", "a1", "legacy-a"}})
	if got := boundedHistoryMessagesForContext(ctx, blobs); len(got) != 2 {
		t.Fatalf("exact lineage not folded: %d", len(got))
	}
}

func TestRequestCountsReasoningAndNamedMessages(t *testing.T) {
	base := llm.Message{Role: "assistant", Content: "observe the target"}
	rich := base
	rich.ReasoningContent = strings.Repeat("prior reasoning ", 1000)
	rich.Name = "observer"
	for _, count := range []func(llm.Message) int{
		func(m llm.Message) int { return NewTokenEstimator().CountMessages([]llm.Message{m}) },
		estimateSingleMessageTokens,
	} {
		if count(rich) <= count(base)+1000 {
			t.Fatal("request accounting omitted reasoning content")
		}
	}
}

func TestProviderRecoveryPreservesRequiredRuntimeContext(t *testing.T) {
	runtime := TaskRuntimeContext{RunID: "current", UserRequirements: []string{"Never publish without my confirmation"}}
	for i := 0; i < 9; i++ {
		runtime.Plan = append(runtime.Plan, PlanItem{StepID: fmt.Sprintf("step-%d", i), Step: strings.Repeat("required milestone ", 30), Status: "pending", SuccessCriteria: "criterion intact"})
	}
	core := "core policy\n" + runtime.requiredPrompt()
	engine := NewContextEngine(200, 20)
	got := engine.RecoverMessages([]llm.Message{{Role: "system", Content: core}, {Role: "user", Content: "continue"}})
	if got[0].Content != core {
		t.Fatal("provider-window recovery silently weakened required context")
	}
	if _, err := engine.PrepareRequest(context.Background(), got, nil); err == nil {
		t.Fatal("oversized mandatory context must remain an actionable error")
	}
}

func TestSelectedPlanSeedsLoopOnContinuation(t *testing.T) {
	for _, status := range []string{"completed", "pending"} {
		t.Run(status, func(t *testing.T) {
			provider := &planEscalationProvider{commands: planGuidanceEscalationThreshold + 1}
			agent := NewAgent(memory.NewMemoryManager(&mockStorage{}), &planEscalationBackend{}, provider, "helpful", 8, 1, nil)
			events := make(chan string, 256)
			ctx := WithTaskRuntimeContext(WithEventChannel(context.Background(), events), TaskRuntimeContext{
				RunID: "current", Plan: []PlanItem{{StepID: "current-step", Step: "check target", Status: status}},
			})
			ctx = WithTaskStrategy(ctx, planEscalationStrategy(PlanPolicyOptional))
			ctx = WithLoopResumeMessages(ctx, []llm.Message{{Role: "user", Content: "check target; do not publish"}})
			if _, _, err := agent.RunConversation(ctx, "person", "cli", "continue"); err != nil {
				t.Fatal(err)
			}
			if requestsContainUserText(provider.requests, requiredPlanWording) {
				t.Fatal("selected durable plan received a missing-plan reminder")
			}
			close(events)
			reconciled := false
			for raw := range events {
				if event, ok := DecodeAgentEvent(raw); ok {
					if event.Type == "strategy.plan_guidance_escalated" {
						t.Fatal("loop ignored the selected durable plan")
					}
					reconciled = reconciled || event.Type == "agent.step" && event.Payload["detail"] == "plan_repair"
				}
			}
			if reconciled != (status == "pending") {
				t.Fatalf("reconciliation=%t for selected status=%s", reconciled, status)
			}
		})
	}
}
