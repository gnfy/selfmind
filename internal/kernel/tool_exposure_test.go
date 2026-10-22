package kernel

import (
	"context"
	"testing"
)

func TestFilterToolDefinitionsAlwaysRemovesHiddenTools(t *testing.T) {
	defs := []map[string]interface{}{
		{"type": "function", "function": map[string]interface{}{"name": "visible"}, "selfmind": map[string]interface{}{"exposure": "direct"}},
		{"type": "function", "function": map[string]interface{}{"name": "lifecycle"}, "selfmind": map[string]interface{}{"exposure": "hidden"}},
	}
	got := filterToolDefinitions(context.Background(), defs, TaskStrategy{})
	if len(got) != 1 || toolDefinitionName(got[0]) != "visible" {
		t.Fatalf("hidden tool leaked into model definitions: %+v", got)
	}
}

func TestDeferredToolsActivateMonotonicallyAfterSearch(t *testing.T) {
	defs := []map[string]interface{}{
		{"type": "function", "function": map[string]interface{}{"name": "direct"}, "selfmind": map[string]interface{}{"exposure": "direct"}},
		{"type": "function", "function": map[string]interface{}{"name": "cold"}, "selfmind": map[string]interface{}{"exposure": "deferred"}},
		{"type": "function", "function": map[string]interface{}{"name": "hidden"}, "selfmind": map[string]interface{}{"exposure": "hidden"}},
	}
	ctx := withToolActivationState(context.Background())
	if got := filterToolDefinitions(ctx, defs, TaskStrategy{}); len(got) != 1 || toolDefinitionName(got[0]) != "direct" {
		t.Fatalf("initial definitions=%+v", got)
	}
	activated := activateToolsFromSearchResult(ctx, "tool_search", `[{"name":"cold","activated":true},{"name":"direct","activated":false}]`)
	if len(activated) != 1 || activated[0] != "cold" {
		t.Fatalf("activated=%v", activated)
	}
	got := filterToolDefinitions(ctx, defs, TaskStrategy{})
	if len(got) != 2 || toolDefinitionName(got[0]) != "direct" || toolDefinitionName(got[1]) != "cold" {
		t.Fatalf("activated definitions=%+v", got)
	}
	if added := activateToolsFromSearchResult(ctx, "tool_search", `[{"name":"cold","activated":true}]`); len(added) != 0 {
		t.Fatalf("repeat activation changed set: %v", added)
	}
}

func TestPagedSkillActivationAlsoActivatesSkillView(t *testing.T) {
	ctx := withToolActivationState(context.Background())
	activated := activateToolsFromSearchResult(ctx, "skill_select", `{"delivery_mode":"paged"}`)
	if len(activated) != 1 || activated[0] != "skill_view" || !deferredToolActive(ctx, "skill_view") {
		t.Fatalf("activated=%v active=%t", activated, deferredToolActive(ctx, "skill_view"))
	}
	if got := activateToolsFromSearchResult(ctx, "skill_select", `{"delivery_mode":"full"}`); len(got) != 0 {
		t.Fatalf("full delivery activated tools: %v", got)
	}
}

func TestPreboundPagedActiveSkillActivatesSkillView(t *testing.T) {
	ctx := WithSkillRuntimeContext(context.Background(), SkillRuntimeContext{
		Active: &ActiveSkillContext{
			Name:                    "openclaw-LoopBench",
			DeliveryMode:            SkillDeliveryModePaged,
			DeliveryContractVersion: 1,
		},
	})
	ctx = withToolActivationState(ctx)
	if !deferredToolActive(ctx, "skill_view") {
		t.Fatal("expected pre-bound paged Active Skill in SkillRuntimeContext to activate skill_view")
	}
}

func TestSkillCandidatesPreActivateSkillViewToPreservePromptCache(t *testing.T) {
	ctx := WithSkillRuntimeContext(context.Background(), SkillRuntimeContext{
		Candidates: []SkillCandidateContext{{Name: "k8s-sre-triage", Description: "Triage workloads"}},
	})
	ctx = withToolActivationState(ctx)
	if !deferredToolActive(ctx, "skill_view") {
		t.Fatal("expected SkillRuntimeContext candidates to pre-activate skill_view before iteration 1")
	}
	if added := activateToolsFromSearchResult(ctx, "skill_select", `{"delivery_mode":"paged"}`); len(added) != 0 {
		t.Fatalf("expected mid-turn skill_select not to mutate active tool set, got %v", added)
	}
}
