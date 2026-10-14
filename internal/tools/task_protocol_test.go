package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type durablePlanProjectionTest struct {
	plan          PlanState
	version       int
	completionErr error
	projectErr    error
}

func (p *durablePlanProjectionTest) Project(_ context.Context, state PlanState) (PlanProjectionResult, error) {
	if p.projectErr != nil {
		return PlanProjectionResult{}, p.projectErr
	}
	p.version++
	for i := range state.Plan {
		if state.Plan[i].StepID == "" {
			state.Plan[i].StepID = "step-issued"
		}
	}
	p.plan = state
	return PlanProjectionResult{Plan: state, Version: p.version, Changed: true}, nil
}

type verificationPreconditionTestError struct{}

func (verificationPreconditionTestError) Error() string                      { return "check missing; prior reuse offered" }
func (verificationPreconditionTestError) PlanVerificationPrecondition() bool { return true }

func TestPlanVerificationRecoveryPreservesExplicitAdoption(t *testing.T) {
	projection := &durablePlanProjectionTest{projectErr: verificationPreconditionTestError{}}
	ctx := WithRunPlanProjection(context.Background(), projection)
	_, err := NewUpdatePlanTool().Execute(map[string]interface{}{
		"plan": []interface{}{map[string]interface{}{"step_id": "owned-step", "status": "completed"}}, "_context": ctx,
	})
	var recovery interface{ ToolRecoveryHint() string }
	if !errors.As(err, &recovery) {
		t.Fatalf("missing structured recovery: %v", err)
	}
	for _, want := range []string{"step_id without a Plan transition", "reuse_prior_verification=true", "reuse_reason", "explanation alone is not adoption"} {
		if !strings.Contains(recovery.ToolRecoveryHint(), want) {
			t.Fatalf("recovery lost %q: %s", want, recovery.ToolRecoveryHint())
		}
	}
	if projection.version != 0 {
		t.Fatal("recovery changed the rejected Plan")
	}
}

func (p *durablePlanProjectionTest) ValidateCompletion(context.Context) error {
	return p.completionErr
}

func TestUpdatePlanToolRecordsStructuredPlan(t *testing.T) {
	tool := NewUpdatePlanTool()
	result, err := tool.Execute(map[string]interface{}{
		"explanation": "starting work",
		"plan": []interface{}{
			map[string]interface{}{"step": "Inspect current state", "status": "completed"},
			map[string]interface{}{"step": "Apply protocol changes", "status": "in_progress"},
			map[string]interface{}{"step": "Verify behavior", "status": "pending"},
		},
		"_tenant_id": "tenant-a",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	var state PlanState
	if err := json.Unmarshal([]byte(result), &state); err != nil {
		t.Fatalf("invalid JSON result: %v", err)
	}
	if got := len(state.Plan); got != 3 {
		t.Fatalf("plan length = %d, want 3", got)
	}
	if state.Plan[1].Status != "in_progress" {
		t.Fatalf("second step status = %q", state.Plan[1].Status)
	}
}

func TestPlanToolsUseDurableProjectionAsCompletionAuthority(t *testing.T) {
	projection := &durablePlanProjectionTest{}
	ctx := WithRunPlanProjection(context.Background(), projection)
	plan := NewUpdatePlanTool()
	result, err := plan.Execute(map[string]interface{}{
		"plan": []interface{}{map[string]interface{}{"step": "Inspect", "status": "completed"}}, "_context": ctx,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"step_id":"step-issued"`) || !strings.Contains(result, `"plan_version":1`) {
		t.Fatalf("durable projection result missing server identity: %s", result)
	}
	projection.completionErr = errors.New("uncertain effect")
	finish := NewFinishRunTool()
	if _, err := finish.Execute(map[string]interface{}{"status": "done", "summary": "done", "_context": ctx}); err == nil || !strings.Contains(err.Error(), "uncertain effect") {
		t.Fatalf("finish_run ignored durable completion authority: %v", err)
	}
}

func TestFinishBodyPreservesParagraphsButCannotOverrideCompletionAuthority(t *testing.T) {
	projection := &durablePlanProjectionTest{}
	ctx := WithRunPlanProjection(context.Background(), projection)
	tool := NewFinishRunTool()
	args := map[string]interface{}{"status": "done", "summary": "checked", "final_response": "First paragraph.\n\nSecond paragraph.", "_context": ctx}
	raw, err := tool.Execute(args)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		FinalResponse string `json:"final_response"`
	}
	if json.Unmarshal([]byte(raw), &out) != nil || out.FinalResponse != args["final_response"] {
		t.Fatalf("body=%q", raw)
	}
	projection.completionErr = errors.New("required check is unresolved")
	if raw, err := tool.Execute(args); err == nil || raw != "" {
		t.Fatalf("prose overrode durable completion: %q %v", raw, err)
	}
}

func TestPlanStepParserPreservesOmittedTextForExactIDInheritance(t *testing.T) {
	steps, err := planStepsFromArgs([]interface{}{map[string]interface{}{
		"step_id": "step-issued", "status": "completed",
		"reuse_prior_verification": true, "reuse_reason": "the same target is unchanged",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].StepID != "step-issued" || steps[0].Step != "" || steps[0].Status != "completed" ||
		!steps[0].ReusePriorVerification || steps[0].ReuseReason != "the same target is unchanged" {
		t.Fatalf("parsed step=%+v", steps)
	}
}

func TestPlanCancellationEvidenceSurvivesParsingAndDeduplication(t *testing.T) {
	steps, err := planStepsFromArgs([]interface{}{map[string]interface{}{
		"step_id": "step-issued", "status": "cancelled",
		"replacement_step_id": "verified-step", "scope_change_quote": "Only prepare the checklist",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].ReplacementStepID != "verified-step" || steps[0].ScopeChangeQuote != "Only prepare the checklist" {
		t.Fatalf("cancellation provenance lost: %+v", steps)
	}
	changed := append([]PlanStep(nil), steps...)
	changed[0].ReplacementStepID = "other-step"
	if samePlanSteps(steps, changed) {
		t.Fatal("a changed evidence reference was deduplicated")
	}
	changed = append([]PlanStep(nil), steps...)
	changed[0].ScopeChangeQuote = "Only inspect local data"
	if samePlanSteps(steps, changed) {
		t.Fatal("a changed user scope quote was deduplicated")
	}
}

func TestUpdatePlanToolReturnsSynchronousWorkUnitIdentities(t *testing.T) {
	tool := NewUpdatePlanTool()
	ctx := WithPlanProjectionSink(context.Background(), func(_ context.Context, steps []PlanStep) ([]PlanWorkUnitIdentity, error) {
		if len(steps) != 2 || steps[1].Step != "B" {
			t.Fatalf("sink received wrong plan: %+v", steps)
		}
		return []PlanWorkUnitIdentity{
			{ID: "wu-a", Sequence: 1, Goal: steps[0].Step, PlanStatus: steps[0].Status},
			{
				ID: "wu-b", Sequence: 2, Goal: steps[1].Step, PlanStatus: steps[1].Status,
				SkillCatalog: "## Skill Candidates for Current Work Unit\n- skref_123 inspect-build: Inspect build metadata.\n",
			},
		}, nil
	})
	result, err := tool.Execute(map[string]interface{}{
		"plan": []interface{}{
			map[string]interface{}{"step": "A", "status": "completed"},
			// related_task_id was removed from the tool surface; an unknown key
			// must be ignored rather than break the call.
			map[string]interface{}{"step": "B", "status": "in_progress", "related_task_id": "task-b"},
		},
		"_context": ctx,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"id":"wu-b"`) || !strings.Contains(result, `"sequence":2`) {
		t.Fatalf("stable work-unit identities missing from tool result: %s", result)
	}
	// related_task_id left the tool surface: it named a Task, it was never used
	// in 28 real calls, and its target is being removed.
	if strings.Contains(result, "related_task_id") {
		t.Fatalf("retired related_task_id resurfaced: %s", result)
	}
	if !strings.Contains(result, `"skill_catalog":"`) || !strings.Contains(result, "skref_123") {
		t.Fatalf("new work-unit Skill refs missing from bounded catalogue: %s", result)
	}
	if strings.Contains(result, `"skill_candidates"`) {
		t.Fatalf("tool result duplicated unbounded candidate descriptions: %s", result)
	}
}

// The model-facing contract asks for the compact form the runtime already
// accepts: every step listed, an unchanged step carried by id and status alone.
// Restating saved text on every update costs output tokens and buys nothing,
// because the receipt returns the full plan.
func TestUpdatePlanContractAsksForCompactUnchangedSteps(t *testing.T) {
	tool := NewUpdatePlanTool()
	schema := tool.Schema().Properties
	item := schema["plan"].Items.Properties
	for name, text := range map[string]string{
		"tool description":      tool.Description(),
		"plan":                  schema["plan"].Description,
		"step":                  item["step"].Description,
		"success_criteria":      item["success_criteria"].Description,
		"verification_required": item["verification_required"].Description,
	} {
		if !strings.Contains(text, "omit") && !strings.Contains(text, "only") && !strings.Contains(text, "alone") {
			t.Errorf("%s does not say what an unchanged step may leave out: %s", name, text)
		}
	}
	if !strings.Contains(tool.Description(), "a step that has not changed needs only its step_id and status") ||
		!strings.Contains(tool.Description(), "returns the full plan") {
		t.Errorf("tool description lost the compact-update contract: %s", tool.Description())
	}
	// The compact form needs ids a result issued, which a first plan cannot
	// have: asking for it unconditionally made models send id-only entries
	// where no step is stored (3 of the 4 first plans after it was introduced
	// were refused for lacking step text, against 2 of 201 before).
	for name, want := range map[string]struct{ text, phrase string }{
		"tool description (first plan has no ids)":    {tool.Description(), "The first plan of a run has no step ids: give every step its text and leave step_id out"},
		"tool description (compact needs issued ids)": {tool.Description(), "Once a result has issued ids"},
		"tool description (nothing to cancel)":        {tool.Description(), "A first plan has nothing to cancel"},
		"plan (text needed in the first plan)":        {schema["plan"].Description, "in the first plan every step needs its text"},
		"step_id (leave it out in a first plan)":      {item["step_id"].Description, "The first plan has none: leave it out"},
		"step (required in the first plan)":           {item["step"].Description, "Required for every step of the first plan"},
	} {
		if !strings.Contains(want.text, want.phrase) {
			t.Errorf("%s lacks %q: %s", name, want.phrase, want.text)
		}
	}
	if strings.Contains(tool.Description(), "An unchanged step needs only its step_id and status") {
		t.Errorf("the compact form must stay conditional on issued ids: %s", tool.Description())
	}
	// The snapshot is still complete: list every step, nothing is a patch.
	for _, want := range []string{"List every step", "not a partial patch"} {
		if !strings.Contains(tool.Description()+schema["plan"].Description, want) {
			t.Errorf("complete-snapshot contract lost %q", want)
		}
	}
	// What the wording promises must be what the runtime does without a Run
	// store: an entry carrying only an id is still refused there, because
	// nothing can supply its text.
	if _, err := tool.Execute(map[string]interface{}{"plan": []interface{}{
		map[string]interface{}{"step_id": "step_a", "status": "completed"},
	}}); err == nil || !strings.Contains(err.Error(), "plan[0].step is required") {
		t.Fatalf("a bare id without a durable plan must still be refused: %v", err)
	}
}

type textRequiredTestError struct {
	index     int
	firstPlan bool
}

func (textRequiredTestError) Error() string { return "plan step needs text" }
func (e textRequiredTestError) PlanStepTextRequired() (int, bool) {
	return e.index, e.firstPlan
}

// An entry without text is refused with the repair that fits where it happened:
// in a first plan no id can have kept text, so the whole plan goes again with
// every step's text; later it is a new step that needs its own text. The code is
// stable and a contract refusal, not an unclassified failure.
func TestUpdatePlanTextlessEntryIsRefusedWithTheRepairForWhereItHappened(t *testing.T) {
	run := func(failure error) (code, category, message, hint string) {
		ctx := WithPlanProjectionSink(context.Background(), func(context.Context, []PlanStep) ([]PlanWorkUnitIdentity, error) {
			return nil, failure
		})
		// The entry has an id and no text, exactly what the runtime has to judge.
		_, err := NewUpdatePlanTool().Execute(map[string]interface{}{
			"plan":     []interface{}{map[string]interface{}{"step_id": "s1", "status": "completed"}},
			"_context": ctx,
		})
		stable, ok := err.(interface {
			ToolErrorCode() string
			ToolErrorCategory() string
			ModelSafeMessage() string
			ToolRecoveryHint() string
		})
		if !ok {
			t.Fatalf("text-required refusal = %T %v", err, err)
		}
		return stable.ToolErrorCode(), stable.ToolErrorCategory(), stable.ModelSafeMessage(), stable.ToolRecoveryHint()
	}

	code, category, message, hint := run(textRequiredTestError{index: 2, firstPlan: true})
	if code != "plan_step_text_required" || category != "invalid_input" {
		t.Errorf("first-plan refusal classified %s/%s", code, category)
	}
	for _, want := range []string{"plan[2].step is required", "this run has no plan yet", "ids are ignored in a first plan"} {
		if !strings.Contains(message, want) {
			t.Errorf("first-plan message lacks %q: %s", want, message)
		}
	}
	if !strings.Contains(hint, "Send the complete plan again: every step with its text, and without step_id.") {
		t.Errorf("first-plan hint = %q", hint)
	}

	code, category, message, hint = run(textRequiredTestError{index: 0})
	if code != "plan_step_text_required" || category != "invalid_input" {
		t.Errorf("later refusal classified %s/%s", code, category)
	}
	if !strings.Contains(message, "plan[0].step is required: with no step_id the entry is a new step") || strings.Contains(message, "no plan yet") {
		t.Errorf("later message = %s", message)
	}
	if !strings.Contains(hint, "echo the exact step_id an earlier update_plan result issued") {
		t.Errorf("later hint = %q", hint)
	}

	// With a durable plan the runtime decides, even for an entry that has neither
	// id nor text: the same typed refusal, not a bare string.
	ctx := WithPlanProjectionSink(context.Background(), func(context.Context, []PlanStep) ([]PlanWorkUnitIdentity, error) {
		return nil, textRequiredTestError{index: 1, firstPlan: true}
	})
	_, err := NewUpdatePlanTool().Execute(map[string]interface{}{
		"plan":     []interface{}{map[string]interface{}{"step": "Edit", "status": "pending"}, map[string]interface{}{"status": "pending"}},
		"_context": ctx,
	})
	var typed interface{ ToolErrorCode() string }
	if !errors.As(err, &typed) || typed.ToolErrorCode() != "plan_step_text_required" {
		t.Fatalf("an entry with no id and no text under a durable plan = %T %v", err, err)
	}
}

type stalePlanStepTestError struct {
	index    int
	supplied string
	ids      []string
	listing  string
}

func (stalePlanStepTestError) Error() string                    { return "plan step does not belong to the run" }
func (e stalePlanStepTestError) CurrentPlanStepIDs() []string   { return e.ids }
func (e stalePlanStepTestError) StalePlanEntry() (int, string)  { return e.index, e.supplied }
func (e stalePlanStepTestError) CurrentPlanStepListing() string { return e.listing }

type staleIdsOnlyTestError struct{ ids []string }

func (staleIdsOnlyTestError) Error() string                  { return "plan step does not belong to the run" }
func (e staleIdsOnlyTestError) CurrentPlanStepIDs() []string { return e.ids }

func TestUpdatePlanStaleStepReceiptMapsIdsToStepsAndLocatesTheEntry(t *testing.T) {
	run := func(t *testing.T, failure error) (string, string) {
		t.Helper()
		ctx := WithPlanProjectionSink(context.Background(), func(context.Context, []PlanStep) ([]PlanWorkUnitIdentity, error) {
			return nil, failure
		})
		_, err := NewUpdatePlanTool().Execute(map[string]interface{}{
			"plan":     []interface{}{map[string]interface{}{"step_id": "step_made_up", "step": "Edit", "status": "in_progress"}},
			"_context": ctx,
		})
		stable, ok := err.(interface {
			ToolErrorCode() string
			ToolErrorCategory() string
			ModelSafeMessage() string
		})
		if !ok || stable.ToolErrorCode() != "stale_plan_step" || stable.ToolErrorCategory() != "stale_precondition" {
			t.Fatalf("stale step recovery = %T %v", err, err)
		}
		return stable.ModelSafeMessage(), err.Error()
	}

	message, _ := run(t, stalePlanStepTestError{
		index: 2, supplied: "step_made_up", ids: []string{"step_a", "step_b"},
		listing: `step_a [completed] "Inspect"; step_b [in_progress] "Edit the parser"`,
	})
	for _, want := range []string{
		`plan[2].step_id "step_made_up" is stale for the current run.`,
		`Current steps (step_id [status] step): step_a [completed] "Inspect"; step_b [in_progress] "Edit the parser".`,
	} {
		if !strings.Contains(message, want) {
			t.Errorf("receipt lacks %q:\n%s", want, message)
		}
	}

	// An error that knows only the ids keeps the plain listing.
	message, _ = run(t, staleIdsOnlyTestError{ids: []string{"step_a", "step_b"}})
	if want := "The supplied step_id is stale for the current run. Current step_id values: step_a, step_b."; !strings.Contains(message, want) {
		t.Errorf("ids-only receipt = %q, want it to contain %q", message, want)
	}

	// Nothing to list: the first snapshot may omit ids; none are invented.
	message, _ = run(t, stalePlanStepTestError{index: 0, supplied: "step_made_up"})
	if !strings.Contains(message, "Omit step_id so this run can create its first durable plan snapshot.") || strings.Contains(message, "Current steps") {
		t.Errorf("empty-plan receipt = %q", message)
	}
}

type staleWorkUnitTestError struct{}

func (staleWorkUnitTestError) Error() string {
	return "work unit wu-old does not belong to run run-current"
}
func (staleWorkUnitTestError) CurrentWorkUnitIDs() []string { return []string{"wu-current"} }

func TestUpdatePlanToolReturnsStableStaleWorkUnitRecovery(t *testing.T) {
	tool := NewUpdatePlanTool()
	ctx := WithPlanProjectionSink(context.Background(), func(context.Context, []PlanStep) ([]PlanWorkUnitIdentity, error) {
		return nil, staleWorkUnitTestError{}
	})
	_, err := tool.Execute(map[string]interface{}{
		"plan": []interface{}{map[string]interface{}{
			"step": "Continue current objective", "status": "in_progress", "work_unit_id": "wu-old",
		}},
		"_context": ctx,
	})
	if err == nil {
		t.Fatal("expected stale work-unit error")
	}
	stable, ok := err.(interface {
		ToolErrorCode() string
		ModelSafeMessage() string
	})
	if !ok || stable.ToolErrorCode() != "stale_work_unit" || !strings.Contains(stable.ModelSafeMessage(), "wu-current") {
		t.Fatalf("stale work-unit recovery = %T %v", err, err)
	}
}

func TestUpdatePlanToolRejectsMultipleInProgressSteps(t *testing.T) {
	tool := NewUpdatePlanTool()
	_, err := tool.Execute(map[string]interface{}{
		"plan": []interface{}{
			map[string]interface{}{"step": "A", "status": "in_progress"},
			map[string]interface{}{"step": "B", "status": "in_progress"},
		},
	})
	if err == nil {
		t.Fatal("expected error for multiple in_progress steps")
	}
}

func TestFinishRunToolNormalizesApprovalStatus(t *testing.T) {
	tool := NewFinishRunTool()
	result, err := tool.Execute(map[string]interface{}{
		"status":       "needs_approval",
		"summary":      "Need confirmation before continuing.",
		"next_steps":   []interface{}{"Approve the proposed action."},
		"need_approve": false,
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("invalid JSON result: %v", err)
	}
	if out["status"] != "blocked" {
		t.Fatalf("status = %q, want blocked", out["status"])
	}
	if out["need_approve"] != true {
		t.Fatalf("need_approve = %v, want true", out["need_approve"])
	}
}

func TestFinishRunToolIgnoresLegacyResolvedBlockerIDs(t *testing.T) {
	// resolved_blocker_ids left the finish_run contract with the task_blockers
	// authority (§10.3). Recorded cassettes still replay calls carrying it, so
	// the tool must accept and drop the argument instead of erroring.
	tool := NewFinishRunTool()
	result, err := tool.Execute(map[string]interface{}{
		"status": "done", "summary": "verified",
		"resolved_blocker_ids": []interface{}{"blocker_one", "blocker_two"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result, "resolved_blocker_ids") {
		t.Fatalf("legacy argument must not survive into the outcome: %s", result)
	}
}

func TestFinishRunToolEmitsCanonicalCompletionReason(t *testing.T) {
	tool := NewFinishRunTool()
	for status, want := range map[string]string{
		"done":             "completed",
		"waiting_external": "waiting_external",
		"waiting_user":     "waiting_user",
		"blocked":          "blocked",
		"failed":           "failed",
	} {
		t.Run(status, func(t *testing.T) {
			result, err := tool.Execute(map[string]interface{}{
				"status": status, "summary": "recorded",
			})
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]interface{}
			if err := json.Unmarshal([]byte(result), &payload); err != nil {
				t.Fatal(err)
			}
			if payload["completion_reason"] != want {
				t.Fatalf("completion_reason=%v, want %q", payload["completion_reason"], want)
			}
		})
	}
}

func TestFinishRunDispatchRejectsUserLifecycleStatuses(t *testing.T) {
	registry := NewRegistry()
	registry.Register(NewFinishRunTool())
	for _, status := range []string{"cancelled", "archived"} {
		t.Run(status, func(t *testing.T) {
			if _, err := registry.Dispatch("finish_run", map[string]interface{}{
				"status": status, "summary": "model tried to end user-owned lifecycle",
			}); err == nil {
				t.Fatalf("finish_run accepted user-only lifecycle status %q", status)
			}
		})
	}
}

func TestFinishRunExecuteRejectsUserLifecycleStatuses(t *testing.T) {
	tool := NewFinishRunTool()
	for _, status := range []string{"cancelled", "archived"} {
		if _, err := tool.Execute(map[string]interface{}{
			"status": status, "summary": "model tried to end user-owned lifecycle",
		}); err == nil {
			t.Fatalf("direct finish_run execution accepted user-only lifecycle status %q", status)
		}
	}
}

func TestFinishRunRequiresResolvedSharedPlan(t *testing.T) {
	store := NewPlanStore()
	plan := NewUpdatePlanToolWithStore(store)
	finish := NewFinishRunToolWithStore(store)
	base := map[string]interface{}{"_tenant_id": "tenant-plan-finalization"}

	_, err := plan.Execute(map[string]interface{}{
		"_tenant_id": base["_tenant_id"],
		"plan": []interface{}{
			map[string]interface{}{"step": "Implement change", "status": "completed"},
			map[string]interface{}{"step": "Run verification", "status": "in_progress"},
		},
	})
	if err != nil {
		t.Fatalf("initial plan update failed: %v", err)
	}

	_, err = finish.Execute(map[string]interface{}{
		"_tenant_id": base["_tenant_id"],
		"status":     "done",
		"summary":    "Completed the work.",
	})
	if err == nil || !strings.Contains(err.Error(), "unresolved plan steps") {
		t.Fatalf("finish_run should reject an unresolved plan, got: %v", err)
	}
	var precondition interface{ ToolErrorCode() string }
	if !errors.As(err, &precondition) || precondition.ToolErrorCode() != "completion_precondition" {
		t.Fatalf("completion rejection has no correction contract: %v", err)
	}

	_, err = plan.Execute(map[string]interface{}{
		"_tenant_id": base["_tenant_id"],
		"plan": []interface{}{
			map[string]interface{}{"step": "Implement change", "status": "completed"},
			map[string]interface{}{"step": "Run verification", "status": "completed"},
		},
	})
	if err != nil {
		t.Fatalf("final plan update failed: %v", err)
	}
	if _, ok := store.Get("tenant-plan-finalization"); ok {
		t.Fatal("resolved plan should be released from the runtime guard store")
	}
	if _, err := finish.Execute(map[string]interface{}{
		"_tenant_id": base["_tenant_id"],
		"status":     "done",
		"summary":    "Completed and verified the work.",
	}); err != nil {
		t.Fatalf("finish_run rejected a resolved plan: %v", err)
	}
	if _, ok := store.last["tenant-plan-finalization"]; ok {
		t.Fatal("finish_run should purge the final plan deduplication snapshot")
	}
}

func TestPlanToolSuppressesIdenticalVisibleSnapshots(t *testing.T) {
	store := NewPlanStore()
	plan := NewUpdatePlanToolWithStore(store)
	args := map[string]interface{}{
		"_tenant_id":  "tenant-plan-dedupe",
		"explanation": "first narration",
		"plan": []interface{}{
			map[string]interface{}{"step": "Inspect", "status": "in_progress"},
			map[string]interface{}{"step": "Verify", "status": "pending"},
		},
	}

	first, err := plan.Execute(args)
	if err != nil {
		t.Fatalf("first update: %v", err)
	}
	var firstResult struct {
		Changed bool `json:"changed"`
	}
	if err := json.Unmarshal([]byte(first), &firstResult); err != nil {
		t.Fatalf("decode first result: %v", err)
	}
	if !firstResult.Changed {
		t.Fatal("first snapshot must be visible")
	}

	args["explanation"] = "different narration"
	second, err := plan.Execute(args)
	if err != nil {
		t.Fatalf("second update: %v", err)
	}
	var secondResult struct {
		Changed bool `json:"changed"`
	}
	if err := json.Unmarshal([]byte(second), &secondResult); err != nil {
		t.Fatalf("decode second result: %v", err)
	}
	if secondResult.Changed {
		t.Fatal("identical steps and statuses must not repaint the plan")
	}
}

// The schema is the model's contract for cancellation evidence. It must match
// what the runtime accepts: a step planned without success_criteria has no
// original criterion to preserve, and a whole short instruction is a quote.
func TestUpdatePlanSchemaStatesTheCancellationEvidenceRules(t *testing.T) {
	step := NewUpdatePlanTool().Schema().Properties["plan"].Items.Properties
	replacement := step["replacement_step_id"].Description
	for _, want := range []string{"established success_criteria", "planned without success_criteria", "states its own"} {
		if !strings.Contains(replacement, want) {
			t.Errorf("replacement_step_id lacks %q: %s", want, replacement)
		}
	}
	if strings.Contains(replacement, "Preserve that original criterion") {
		t.Errorf("replacement_step_id demands a criterion a criteria-less step lacks: %s", replacement)
	}
	for _, field := range []string{"scope_change_quote", "user_takeover_quote"} {
		description := step[field].Description
		for _, want := range []string{"whole user message", "whole sentence", "at least two words", "any part of a question"} {
			if !strings.Contains(description, want) {
				t.Errorf("%s lacks %q: %s", field, want, description)
			}
		}
	}
}
