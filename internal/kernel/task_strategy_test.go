package kernel

import (
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
)

func TestTaskStrategyDefaultIsAgentFirst(t *testing.T) {
	strategy := BuildTaskStrategy("analyze this repository and suggest improvements", "cli")

	if strategy.Class != TaskClassGeneralTask {
		t.Fatalf("class = %s, want %s", strategy.Class, TaskClassGeneralTask)
	}
	if strategy.ToolMode != ToolModeFull {
		t.Fatalf("tool mode = %s, want %s", strategy.ToolMode, ToolModeFull)
	}
	if strategy.PlanPolicy != PlanPolicyOptional {
		t.Fatalf("plan policy = %s, want %s", strategy.PlanPolicy, PlanPolicyOptional)
	}
	if !strategy.AllowsTool("read_file") || !strategy.AllowsTool("write_file") || !strategy.AllowsTool("terminal") {
		t.Fatalf("agent-first policy should leave local tools available: %+v", strategy)
	}
	if !strategy.AllowsTool("update_plan") {
		t.Fatalf("agent-first policy should leave optional planning available: %+v", strategy)
	}
	if strategy.AllowsTool("web_search") || strategy.AllowsTool("web_extract") {
		t.Fatalf("web tools should stay hidden until explicitly requested: %+v", strategy)
	}
}

func TestTaskStrategyPureModelQuestionIsSoftHintNotToolHiding(t *testing.T) {
	// Codex philosophy: a likely direct-answer turn is a SOFT hint (fast-model
	// routing + no plan), not a reason to strip the tool surface. Local tools
	// stay available so a misclassified "needs a tool" turn is not crippled.
	strategy := BuildTaskStrategy("\u4f60\u662f\u4ec0\u4e48\u6a21\u578b\uff1f", "cli")

	if strategy.Class != TaskClassSimpleAnswer {
		t.Fatalf("class = %s, want %s", strategy.Class, TaskClassSimpleAnswer)
	}
	if strategy.ToolMode != ToolModeFull {
		t.Fatalf("tool mode = %s, want %s (tools stay exposed)", strategy.ToolMode, ToolModeFull)
	}
	if strategy.PlanPolicy != PlanPolicyDisabled {
		t.Fatalf("plan policy = %s, want %s for a trivial turn", strategy.PlanPolicy, PlanPolicyDisabled)
	}
	if !strategy.AllowsTool("read_file") || !strategy.AllowsTool("terminal") {
		t.Fatalf("direct-answer hint must not strip local tools: %+v", strategy)
	}
	if strategy.AllowsTool("update_plan") {
		t.Fatalf("planning should be hidden when plan policy is disabled: %+v", strategy)
	}
	if strategy.AllowsTool("web_search") {
		t.Fatalf("web tools should stay hidden until explicitly requested: %+v", strategy)
	}
}

func TestTaskStrategyCodingExampleKeepsAgentFirstToolsOptional(t *testing.T) {
	strategy := BuildTaskStrategy("\u7528 PHP \u5b9e\u73b0\u4e00\u4e2a pgsql \u64cd\u4f5c\u793a\u4f8b", "cli")

	if strategy.Class != TaskClassCodingExample {
		t.Fatalf("class = %s, want %s", strategy.Class, TaskClassCodingExample)
	}
	if !strategy.AllowsTool("read_file") || !strategy.AllowsTool("write_file") || !strategy.AllowsTool("update_plan") {
		t.Fatalf("coding examples should keep agent-first optional tools available: %+v", strategy)
	}
}

func TestTaskStrategyExplicitLookupEnablesWebWithoutHidingLocalTools(t *testing.T) {
	strategy := BuildTaskStrategy("look up the latest stable Go version", "cli")

	if strategy.Class != TaskClassExternalLookup {
		t.Fatalf("class = %s, want %s", strategy.Class, TaskClassExternalLookup)
	}
	if !strategy.AllowsTool("web_search") || !strategy.AllowsTool("web_extract") {
		t.Fatalf("explicit lookup should expose web tools: %+v", strategy)
	}
	if !strategy.AllowsTool("read_file") || !strategy.AllowsTool("terminal") {
		t.Fatalf("explicit lookup should not remove local tools from the agent: %+v", strategy)
	}
	if !strategy.AllowsTool("update_plan") {
		t.Fatalf("explicit lookup should keep optional planning available: %+v", strategy)
	}
}

func TestTaskStrategyEmptyInputFallsBackToAgentFirstDefault(t *testing.T) {
	// Empty input is degenerate (the gateway guards it), but it must not invent
	// a tool-hiding classification. Fall back to the agent-first default.
	strategy := BuildTaskStrategy("   ", "cli")

	if strategy.ToolMode != ToolModeFull {
		t.Fatalf("tool mode = %s, want %s", strategy.ToolMode, ToolModeFull)
	}
	if !strategy.AllowsTool("read_file") {
		t.Fatalf("empty input should still fall back to the default tool surface: %+v", strategy)
	}
	if strategy.AllowsTool("web_search") {
		t.Fatalf("web tools should stay hidden by default: %+v", strategy)
	}
}

func TestTaskStrategyUsesFiniteElasticBudgetWithoutInventingLifecycleTools(t *testing.T) {
	strategy := BuildTaskStrategy("deploy the service and wait for CI", "cli")
	if strategy.MaxActionTools != 10 || strategy.ActionToolBudgetLimit != 34 || strategy.MaxBudgetExtensions != 6 {
		t.Fatalf("budget = initial:%d limit:%d extensions:%d", strategy.MaxActionTools, strategy.ActionToolBudgetLimit, strategy.MaxBudgetExtensions)
	}
	note := strategy.SystemPromptNote()
	for _, unavailable := range []string{"watch_external", "update_plan", "finish_run"} {
		if strings.Contains(note, unavailable) {
			t.Fatalf("generic strategy note invented capability %q: %q", unavailable, note)
		}
	}
}

func TestToolBudgetPolicyAppliesGenericEvidenceGatedEnvelope(t *testing.T) {
	policy := ToolBudgetPolicy{
		Initial:       12,
		Step:          6,
		Limit:         64,
		MaxExtensions: 9,
	}
	strategy := policy.apply(BuildTaskStrategy("inspect this repository and diagnose the failure", "cli"))

	if strategy.MaxActionTools != 12 {
		t.Fatalf("initial budget = %d, want 12", strategy.MaxActionTools)
	}
	if strategy.ActionToolBudgetStep != 6 {
		t.Fatalf("budget step = %d, want 6", strategy.ActionToolBudgetStep)
	}
	if strategy.ActionToolBudgetLimit != 64 {
		t.Fatalf("budget limit = %d, want 64", strategy.ActionToolBudgetLimit)
	}
	if strategy.MaxBudgetExtensions != 9 {
		t.Fatalf("extensions = %d, want 9", strategy.MaxBudgetExtensions)
	}
}

func TestToolBudgetPolicyDoesNotEnableToolsForToolFreeStrategy(t *testing.T) {
	strategy := TaskStrategy{
		ToolMode:              ToolModeNone,
		MaxActionTools:        0,
		ActionToolBudgetLimit: 0,
		MaxBudgetExtensions:   0,
	}
	got := (ToolBudgetPolicy{Initial: 12, Step: 6, Limit: 64, MaxExtensions: 9}).apply(strategy)

	if got.MaxActionTools != 0 || got.ActionToolBudgetLimit != 0 || got.MaxBudgetExtensions != 0 {
		t.Fatalf("tool-free strategy was expanded: %+v", got)
	}
}

func TestToolBudgetPolicyClampsInitialBudgetToLimit(t *testing.T) {
	strategy := (ToolBudgetPolicy{Initial: 100, Limit: 40}).apply(DefaultTaskStrategy())
	if strategy.MaxActionTools != 40 || strategy.ActionToolBudgetLimit != 40 {
		t.Fatalf("budget = initial:%d limit:%d, want 40/40", strategy.MaxActionTools, strategy.ActionToolBudgetLimit)
	}
}

func TestFilterToolCallsByStrategyOnlyBlocksHiddenWebCalls(t *testing.T) {
	strategy := BuildTaskStrategy("write a PHP pgsql operation example", "cli")
	calls := []llm.ToolCall{
		{Function: "read_file"},
		{Function: "update_plan"},
		{Function: "web_search"},
	}

	got := filterToolCallsByStrategy(calls, strategy)
	if len(got) != 2 || got[0].Function != "read_file" || got[1].Function != "update_plan" {
		t.Fatalf("local and planning tools should pass while hidden web calls are filtered: %+v", got)
	}
}

func TestFilterToolCallsByLifecycleCaps(t *testing.T) {
	calls := []llm.ToolCall{
		{Function: "update_plan"},
		{Function: "finish_run"},
		{Function: "read_file"},
	}
	got, dropped := filterToolCallsByLifecycleCaps(calls, map[string]int{
		"update_plan": 8,
		"finish_run":  1,
	})
	if dropped != 2 {
		t.Fatalf("dropped = %d, want 2", dropped)
	}
	if len(got) != 1 || got[0].Function != "read_file" {
		t.Fatalf("got = %+v, want only read_file", got)
	}
}

func TestFilterToolCallsByLifecycleCapsConsumesCurrentBatch(t *testing.T) {
	calls := []llm.ToolCall{
		{Function: "update_plan"},
		{Function: "update_plan"},
		{Function: "finish_run"},
		{Function: "finish_run"},
		{Function: "read_file"},
	}
	got, dropped := filterToolCallsByLifecycleCaps(calls, map[string]int{
		"update_plan": 7,
	})
	if dropped != 2 {
		t.Fatalf("dropped = %d, want 2", dropped)
	}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3: %+v", len(got), got)
	}
	if got[0].Function != "update_plan" || got[1].Function != "finish_run" || got[2].Function != "read_file" {
		t.Fatalf("got = %+v, want one update_plan, one finish_run, and read_file", got)
	}
}

func TestUpdatePlanLifecycleCapSupportsMeaningfulProgress(t *testing.T) {
	if got := lifecycleToolCap("update_plan"); got != 8 {
		t.Fatalf("update_plan lifecycle cap = %d, want 8", got)
	}
	calls := make([]llm.ToolCall, 9)
	for i := range calls {
		calls[i] = llm.ToolCall{Function: "update_plan"}
	}
	got, dropped := filterToolCallsByLifecycleCaps(calls, map[string]int{})
	if len(got) != 8 || dropped != 1 {
		t.Fatalf("len(got)=%d dropped=%d, want 8 and 1", len(got), dropped)
	}
}

func TestWorkSelectLifecycleCapAllowsOneAuditedCorrection(t *testing.T) {
	if got := lifecycleToolCap("work_select"); got != 2 {
		t.Fatalf("work_select lifecycle cap = %d, want proposal plus one correction", got)
	}
	calls := []llm.ToolCall{{Function: "work_select"}, {Function: "work_select"}, {Function: "work_select"}}
	got, dropped := filterToolCallsByLifecycleCaps(calls, map[string]int{})
	if len(got) != 2 || dropped != 1 {
		t.Fatalf("len(got)=%d dropped=%d, want 2 and 1", len(got), dropped)
	}
}

func TestUnresolvedPlanStepsFromToolCall(t *testing.T) {
	call := llm.ToolCall{Function: "update_plan", Args: `{"plan":[{"step":"inspect","status":"completed"},{"step":"verify","status":"in_progress"},{"step":"optional cleanup","status":"cancelled"}]}`}
	got, ok := unresolvedPlanStepsFromToolCall(call)
	if !ok || len(got) != 1 || got[0] != "verify" {
		t.Fatalf("unresolved plan = %v, ok=%v; want [verify], true", got, ok)
	}
}

func TestFinishRunStatusFromToolCall(t *testing.T) {
	got, ok := finishRunStatusFromToolCall(llm.ToolCall{Function: "finish_run", Args: `{"status":"waiting_external"}`})
	if !ok || got != "waiting_external" {
		t.Fatalf("finish status = %q, ok=%v", got, ok)
	}
}

func TestPlanEvidenceExcludesLifecycleAndReadOnlyTools(t *testing.T) {
	// Only substantive work counts as evidence that a Run is multi-step.
	// Lifecycle bookkeeping is not work, and a read-only observation may still
	// belong to a direct answer.
	for _, name := range []string{"update_plan", "finish_run", "queue_user_input", "work_select"} {
		if countsTowardPlanEvidence(name) {
			t.Fatalf("lifecycle tool %q must not count as plan evidence", name)
		}
	}
	for _, name := range []string{"read_file", "search_files", "list_files", "process_poll"} {
		if countsTowardPlanEvidence(name) {
			t.Fatalf("read-only tool %q must not count as plan evidence", name)
		}
	}
	for _, name := range []string{"terminal", "write_file", "patch", "some_unregistered_tool"} {
		if !countsTowardPlanEvidence(name) {
			t.Fatalf("substantive tool %q must count as plan evidence", name)
		}
	}
}

func TestPlanGuidanceEscalatesOnlyAfterEnoughInRunEvidence(t *testing.T) {
	if planGuidanceEscalationThreshold != 2 {
		t.Fatalf("plan escalation threshold=%d, want 2 so guidance arrives before a third substantive action", planGuidanceEscalationThreshold)
	}
	strategy := DefaultTaskStrategy()
	if strategy.PlanPolicy != PlanPolicyOptional {
		t.Fatalf("precondition: default plan policy = %s", strategy.PlanPolicy)
	}
	if shouldEscalatePlanGuidance(strategy, planGuidanceEscalationThreshold-1, false) {
		t.Fatal("guidance escalated below the evidence threshold")
	}
	if !shouldEscalatePlanGuidance(strategy, planGuidanceEscalationThreshold, false) {
		t.Fatal("guidance did not escalate after the evidence threshold was reached")
	}
}

func TestPlanGuidanceNotEscalatedWhenRunAlreadyHasPlan(t *testing.T) {
	if shouldEscalatePlanGuidance(DefaultTaskStrategy(), planGuidanceEscalationThreshold+10, true) {
		t.Fatal("a Run with a durable plan must not be escalated")
	}
}

func TestPlanGuidanceNeverEscalatesDisabledOrToolFreeTurns(t *testing.T) {
	direct := BuildTaskStrategy("你是什么模型？", "cli")
	if direct.PlanPolicy != PlanPolicyDisabled {
		t.Fatalf("precondition: plan policy = %s, want disabled", direct.PlanPolicy)
	}
	if shouldEscalatePlanGuidance(direct, planGuidanceEscalationThreshold+10, false) {
		t.Fatal("a direct-answer turn must never be escalated to a required plan")
	}
	if escalated := direct.WithPlanRequired(); escalated.PlanPolicy != PlanPolicyDisabled || escalated.AllowsTool("update_plan") {
		t.Fatalf("WithPlanRequired must leave a disabled turn disabled with update_plan hidden: %+v", escalated)
	}

	toolFree := DefaultTaskStrategy()
	toolFree.ToolMode = ToolModeNone
	if shouldEscalatePlanGuidance(toolFree, planGuidanceEscalationThreshold+10, false) {
		t.Fatal("a tool-free turn must never be escalated")
	}

	hidden := DefaultTaskStrategy().WithHiddenTools("update_plan")
	if shouldEscalatePlanGuidance(hidden, planGuidanceEscalationThreshold+10, false) {
		t.Fatal("guidance must not require a tool this turn cannot call")
	}
}

func TestWithPlanRequiredEscalatesWithoutWideningToolSurface(t *testing.T) {
	escalated := DefaultTaskStrategy().WithPlanRequired()
	if escalated.PlanPolicy != PlanPolicyRequired {
		t.Fatalf("plan policy = %s, want required", escalated.PlanPolicy)
	}
	if !escalated.AllowsTool("update_plan") {
		t.Fatalf("required planning must keep update_plan available: %+v", escalated)
	}
	if escalated.AllowsTool("web_search") || escalated.AllowsTool("web_extract") {
		t.Fatalf("plan escalation must not un-hide web tools: %+v", escalated)
	}
	if shouldEscalatePlanGuidance(escalated, planGuidanceEscalationThreshold+10, false) {
		t.Fatal("an already-required turn has nothing left to escalate")
	}
}

func TestPlanGuidanceEscalationNudgeCarriesRequiredWording(t *testing.T) {
	nudge := planGuidanceEscalationNudge(DefaultTaskStrategy(), 5)
	required := planToolGuidance(DefaultTaskStrategy().WithPlanRequired())
	if !strings.Contains(nudge, strings.TrimSpace(required)) {
		t.Fatalf("nudge does not carry the required plan wording:\n%s", nudge)
	}
	optional := planToolGuidance(DefaultTaskStrategy())
	if strings.Contains(nudge, "BEFORE the first action tool") {
		t.Fatalf("nudge repeated the optional wording it is meant to supersede:\n%s\noptional:\n%s", nudge, optional)
	}
	if !strings.Contains(nudge, "5 substantive tool action") {
		t.Fatalf("nudge does not state the observed in-run evidence:\n%s", nudge)
	}
	if !strings.Contains(nudge, "finish normally") {
		t.Fatalf("nudge must leave completion available:\n%s", nudge)
	}
}

func TestWithWebEnabledUnhidesWebTools(t *testing.T) {
	// Default strategy keeps web tools hidden; WithWebEnabled must un-hide them
	// and flip the policy, without touching local tools.
	s := DefaultTaskStrategy()
	if s.AllowsTool("web_search") {
		t.Fatal("precondition: web should be hidden by default")
	}
	s = s.WithWebEnabled()
	if !s.AllowsTool("web_search") || !s.AllowsTool("web_extract") {
		t.Fatalf("web tools should be available after WithWebEnabled: %+v", s)
	}
	if s.WebPolicy != WebPolicyEnabled {
		t.Fatalf("web policy = %s, want enabled", s.WebPolicy)
	}
	if !s.AllowsTool("read_file") {
		t.Fatalf("local tools must remain available: %+v", s)
	}
}

// The plan guidance must carry the step-transition discipline under BOTH
// policies. Without it a reluctant model produced its first snapshot only
// after the work was done, so the person's first sight of the plan was
// several steps retroactively marked completed (observed live 2026-09-03).
func TestPlanGuidanceCarriesStepDisciplineUnderBothPolicies(t *testing.T) {
	optional := planToolGuidance(DefaultTaskStrategy())
	required := planToolGuidance(DefaultTaskStrategy().WithPlanRequired())
	for name, guidance := range map[string]string{"optional": optional, "required": required} {
		for _, want := range []string{
			"in_progress before you work on it",
			"never jump a step straight from pending to completed",
			"never batch-complete several steps after the fact",
			"resolve every step before a done outcome",
		} {
			if !strings.Contains(guidance, want) {
				t.Fatalf("%s guidance is missing %q:\n%s", name, want, guidance)
			}
		}
	}
	// Concrete triggers replace the abstract test, and they belong only to the
	// policy where the model still has to decide.
	for _, want := range []string{
		"ordered phases or dependencies",
		"answers more than one request at once",
		"grows extra steps while you work",
	} {
		if !strings.Contains(optional, want) {
			t.Fatalf("optional guidance is missing the trigger %q:\n%s", want, optional)
		}
		if strings.Contains(required, want) {
			t.Fatalf("required guidance must not restate decision triggers (%q):\n%s", want, required)
		}
	}
	if guidance := planToolGuidance(planEscalationStrategy(PlanPolicyDisabled)); !strings.Contains(guidance, "Do not call update_plan") {
		t.Fatalf("a disabled turn keeps its refusal: %s", guidance)
	}
}
