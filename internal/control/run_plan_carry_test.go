package control

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"selfmind/internal/verification"
)

// carryShape describes a parent Run's Plan: which steps are completed, which
// completed required steps have a passing check bound to them, and which step is
// active. Step i requires verification when required[i] is set.
type carryShape struct {
	required  []bool
	completed []int
	checked   []int
	active    int // index of the in-progress step, or -1
}

type carryRun struct {
	store    *Store
	identity *IdentityContext
	task     *Task
	run      *Run
	stepIDs  []string
}

func contains(values []int, want int) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func newCarryParent(t *testing.T, shape carryShape) carryRun {
	t.Helper()
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	inputs := make([]RunPlanStepInput, len(shape.required))
	for i := range inputs {
		inputs[i] = RunPlanStepInput{Step: fmt.Sprintf("Step %c", 'A'+i), Status: "pending", SuccessCriteria: fmt.Sprintf("condition %c holds", 'A'+i), VerificationRequired: shape.required[i]}
	}
	inputs[0].Status = "in_progress"
	first, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "initial", inputs)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(inputs))
	for i, step := range first.Plan.Steps {
		ids[i] = step.StepID
	}
	for _, i := range shape.checked {
		binding, err := store.ResolveVerificationBinding(ctx, identity.TenantID, run.ID, verification.Binding{StepID: ids[i]}, "/workspace")
		if err != nil || binding == nil {
			t.Fatalf("binding for step %d: %+v %v", i, binding, err)
		}
		appendPlanEvidence(t, store, run.ID, fmt.Sprintf("check-%d", i), binding, int64(10+10*i))
	}
	next := make([]RunPlanStepInput, len(inputs))
	for i := range next {
		status := "pending"
		switch {
		case contains(shape.completed, i):
			status = "completed"
		case i == shape.active:
			status = "in_progress"
		}
		next[i] = RunPlanStepInput{StepID: ids[i], Status: status}
	}
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "progress", next); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	return carryRun{store: store, identity: identity, task: task, run: run, stepIDs: ids}
}

func (c carryRun) continueRun(t *testing.T, from *Run, exact bool) *Run {
	t.Helper()
	child, err := c.store.StartRunWithOptions(context.Background(), c.task, "cli", "continue", StartRunOptions{ResumesRunID: from.ID, ExactContinuation: exact})
	if err != nil {
		t.Fatal(err)
	}
	return child
}

// recordMutation appends a file change to a run's evidence, after the check that
// the carried steps stand on.
func (c carryRun) recordMutation(t *testing.T, run *Run) {
	t.Helper()
	mutation, _ := json.Marshal(map[string]interface{}{"evidence": map[string]interface{}{
		"kind": "mutation", "tool_name": "write_file", "tool_call_id": "write-1", "status": "succeeded",
		"started_at_unix_nano": int64(500), "finished_at_unix_nano": int64(501),
		"files": []map[string]interface{}{{"path": "/workspace/state.json", "before_sha256": "aa", "after_sha256": "bb"}},
	}})
	if _, err := c.store.AppendEvent(context.Background(), Event{RunID: run.ID, Type: "evidence.recorded", Payload: mutation}); err != nil {
		t.Fatal(err)
	}
}

func (c carryRun) planShape(t *testing.T, run *Run) string {
	t.Helper()
	plan, err := c.store.LatestRunPlan(context.Background(), c.identity.TenantID, run.ID)
	if err != nil || plan == nil {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	var shape strings.Builder
	for _, step := range plan.Steps {
		switch step.Status {
		case "completed":
			shape.WriteString("C")
		case "in_progress":
			shape.WriteString(">")
		case "pending":
			shape.WriteString(".")
		default:
			shape.WriteString("x")
		}
	}
	return shape.String()
}

// RUQX-1272: three verified steps and an active one, a clarification answered, and
// the continuation's Plan started over as [>...] although the parent had [CCC>].
func TestExactContinuationKeepsVerifiedProgressWithItsEvidence(t *testing.T) {
	c := newCarryParent(t, carryShape{required: []bool{true, true, true, false}, completed: []int{0, 1, 2}, checked: []int{0, 1, 2}, active: 3})
	child := c.continueRun(t, c.run, true)
	if got := c.planShape(t, child); got != "CCC>" {
		t.Fatalf("an exact continuation lost the parent's progress: [%s]", got)
	}
	plan, _ := c.store.LatestRunPlan(context.Background(), c.identity.TenantID, child.ID)
	for i := 0; i < 3; i++ {
		step := plan.Steps[i]
		if !step.ReusePriorVerification || step.ReuseReason != carriedVerificationReason || step.StepID != c.stepIDs[i] || step.SourceStepID != c.stepIDs[i] {
			t.Fatalf("step %d was not carried with its provenance: %+v", i, step)
		}
	}
	if plan.Steps[3].ReusePriorVerification {
		t.Fatal("a step that needs no verification was marked as adopted")
	}
	prior, err := c.store.ListInheritedPlanEvidence(context.Background(), c.identity.TenantID, child.ID)
	if err != nil || len(prior) != 3 {
		t.Fatalf("carried steps lost their source evidence: %+v %v", prior, err)
	}
	for _, item := range prior {
		if item.PriorVerification != "passed" || item.ReuseBlockedReason != "" {
			t.Fatalf("carried evidence is not valid: %+v", item)
		}
	}
	// Carrying grants nothing: the unfinished step still keeps the Run from completing.
	if err := c.store.ValidateRunCompletion(context.Background(), c.identity.TenantID, child.ID); err == nil {
		t.Fatal("a carried plan with an active step completed")
	}
}

// A person's turn or a continuation Main selected keeps the conservative import:
// verified steps reopen until Main adopts or rechecks them.
func TestOtherContinuationsKeepTheConservativeImport(t *testing.T) {
	c := newCarryParent(t, carryShape{required: []bool{true, true, true, false}, completed: []int{0, 1, 2}, checked: []int{0, 1, 2}, active: 3})
	child := c.continueRun(t, c.run, false)
	if got := c.planShape(t, child); got != ">..." {
		t.Fatalf("a continuation that is not exact changed its import: [%s]", got)
	}
	plan, _ := c.store.LatestRunPlan(context.Background(), c.identity.TenantID, child.ID)
	for _, step := range plan.Steps {
		if step.ReusePriorVerification {
			t.Fatalf("adoption was granted without Main: %+v", step)
		}
	}
}

// Only a step whose source no longer validates is reopened, alone; a step Main
// completed without any bound check has nothing to carry.
func TestExactContinuationReopensOnlyTheStepsWithoutValidSource(t *testing.T) {
	c := newCarryParent(t, carryShape{required: []bool{true, true, true, false}, completed: []int{0, 1, 2}, checked: []int{0, 2}, active: 3})
	child := c.continueRun(t, c.run, true)
	if got := c.planShape(t, child); got != "C.C>" {
		t.Fatalf("expected only the unproven step to reopen, got [%s]", got)
	}
	plan, _ := c.store.LatestRunPlan(context.Background(), c.identity.TenantID, child.ID)
	if plan.Steps[1].ReusePriorVerification || !plan.Steps[0].ReusePriorVerification || !plan.Steps[2].ReusePriorVerification {
		t.Fatalf("adoption flags are wrong: %+v", plan.Steps)
	}
}

// With nothing active in the parent, the earliest reopened step becomes the active
// one, as a first plan's earliest unverified step does. A step Main completed
// without any check, in a work unit that is still open, is the reopened one.
func TestExactContinuationActivatesTheEarliestReopenedStepWhenNothingIsActive(t *testing.T) {
	c := newCarryParent(t, carryShape{required: []bool{true, true, true}, completed: []int{0, 1}, checked: []int{0}, active: -1})
	if got := c.planShape(t, c.run); got != "CC." {
		t.Fatalf("fixture: parent shape [%s]", got)
	}
	child := c.continueRun(t, c.run, true)
	if got := c.planShape(t, child); got != "C>." {
		t.Fatalf("got [%s], want the first unproven step active and the later one pending", got)
	}
}

// Every approval answer is a hop. The progress, and the adoption behind it, must
// survive any number of them while the Plan's single work unit is still open.
func TestCarriedProgressSurvivesRepeatedExactContinuations(t *testing.T) {
	c := newCarryParent(t, carryShape{required: []bool{true, true, false}, completed: []int{0, 1}, checked: []int{0, 1}, active: 2})
	run := c.run
	for hop := 1; hop <= 8; hop++ {
		run = c.continueRun(t, run, true)
		if got := c.planShape(t, run); got != "CC>" {
			t.Fatalf("hop %d lost progress: [%s]", hop, got)
		}
		if err := c.store.FinishRun(context.Background(), c.identity.TenantID, run.ID, "waiting_user"); err != nil {
			t.Fatal(err)
		}
	}
	child := c.continueRun(t, run, true)
	if _, err := c.store.SyncRunPlan(context.Background(), c.identity.TenantID, child.ID, "finished", []RunPlanStepInput{
		{StepID: c.stepIDs[0], Status: "completed"}, {StepID: c.stepIDs[1], Status: "completed"}, {StepID: c.stepIDs[2], Status: "completed"},
	}); err != nil {
		t.Fatalf("carried adoption was refused when the last step completed: %v", err)
	}
	if err := c.store.ValidateRunCompletion(context.Background(), c.identity.TenantID, child.ID); err != nil {
		t.Fatalf("the carried verification could not complete the Run: %v", err)
	}
}

// A change recorded in a run between the original check and this one can affect
// what the check read. It invalidates the carry like a change in the first or last run.
func TestAChangeInAnIntermediateRunInvalidatesTheCarry(t *testing.T) {
	c := newCarryParent(t, carryShape{required: []bool{true, false}, completed: []int{0}, checked: []int{0}, active: 1})
	middle := c.continueRun(t, c.run, true)
	if got := c.planShape(t, middle); got != "C>" {
		t.Fatalf("first hop: [%s]", got)
	}
	c.recordMutation(t, middle)
	if err := c.store.FinishRun(context.Background(), c.identity.TenantID, middle.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	last := c.continueRun(t, middle, true)
	// The parent's active step stays active; the step whose observation the
	// change can have affected reopens, alone.
	if got := c.planShape(t, last); got != ".>" {
		t.Fatalf("a change made in the middle run did not invalidate the carry: [%s]", got)
	}
	plan, _ := c.store.LatestRunPlan(context.Background(), c.identity.TenantID, last.ID)
	if plan.Steps[0].ReusePriorVerification {
		t.Fatalf("a reopened step kept its adoption: %+v", plan.Steps[0])
	}
}

// A parent that has no plan, or only steps that need no verification, imports as before.
func TestExactContinuationOfUnverifiedStepsIsUnchanged(t *testing.T) {
	c := newCarryParent(t, carryShape{required: []bool{false, false, false}, completed: []int{0}, checked: nil, active: 1})
	child := c.continueRun(t, c.run, true)
	if got := c.planShape(t, child); got != "C>." {
		t.Fatalf("got [%s]", got)
	}
}

// A run the carry skips over still has to share the execution scope: a reference
// that points past it may shorten the walk, never widen what the observation may
// be taken from.
func TestACarrySkippingARunInAnotherScopeIsRefused(t *testing.T) {
	c := newCarryParent(t, carryShape{required: []bool{true, false}, completed: []int{0}, checked: []int{0}, active: 1})
	first := c.continueRun(t, c.run, true)
	if err := c.store.FinishRun(context.Background(), c.identity.TenantID, first.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	second := c.continueRun(t, first, true)
	if got := c.planShape(t, second); got != "C>" {
		t.Fatalf("second hop: [%s]", got)
	}
	if err := c.store.FinishRun(context.Background(), c.identity.TenantID, second.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.store.db.ExecContext(context.Background(), `UPDATE runs SET workspace_id='ws-elsewhere' WHERE id=?`, first.ID); err != nil {
		t.Fatal(err)
	}
	third := c.continueRun(t, second, true)
	if got := c.planShape(t, third); got != ".>" {
		t.Fatalf("an observation spanning another scope was carried: [%s]", got)
	}
}

// A chain longer than the bound degrades to the conservative import instead of
// failing the continuation: the Run always starts, with the steps reopened.
func TestACarryPastTheLineageBoundReopensInsteadOfFailing(t *testing.T) {
	c := newCarryParent(t, carryShape{required: []bool{true, false}, completed: []int{0}, checked: []int{0}, active: 1})
	run := c.run
	carried := 0
	for hop := 1; hop <= maxReuseLineage+3; hop++ {
		run = c.continueRun(t, run, true)
		if c.planShape(t, run) == "C>" {
			carried++
		}
		if err := c.store.FinishRun(context.Background(), c.identity.TenantID, run.ID, "waiting_user"); err != nil {
			t.Fatal(err)
		}
	}
	if carried < maxReuseLineage-1 || carried > maxReuseLineage+1 {
		t.Fatalf("carried %d hops, want about the lineage bound (%d)", carried, maxReuseLineage)
	}
	if got := c.planShape(t, run); got != ".>" {
		t.Fatalf("past the bound the step should have reopened, got [%s]", got)
	}
}

// A reference that points past a run does not excuse it: a change recorded in a
// run the carry skips, after the continuation that follows it already started
// (a background process finishing late), still invalidates the observation.
func TestARunSkippedByTheCarryIsStillCheckedForChanges(t *testing.T) {
	c := newCarryParent(t, carryShape{required: []bool{true, false}, completed: []int{0}, checked: []int{0}, active: 1})
	first := c.continueRun(t, c.run, true)
	if err := c.store.FinishRun(context.Background(), c.identity.TenantID, first.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	second := c.continueRun(t, first, true)
	if err := c.store.FinishRun(context.Background(), c.identity.TenantID, second.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	c.recordMutation(t, first)
	third := c.continueRun(t, second, true)
	if got := c.planShape(t, third); got != ".>" {
		t.Fatalf("a change in a skipped run did not invalidate the carry: [%s]", got)
	}
}

func (c carryRun) recheck(t *testing.T, run *Run, step int, id string, at int64) {
	t.Helper()
	binding, err := c.store.ResolveVerificationBinding(context.Background(), c.identity.TenantID, run.ID, verification.Binding{StepID: c.stepIDs[step]}, "/workspace")
	if err != nil || binding == nil {
		t.Fatalf("binding for step %d: %+v %v", step, binding, err)
	}
	appendObservedCheck(t, c.store, run.ID, id, binding, at)
}

func (c carryRun) finishAll(t *testing.T, run *Run) (RunPlanProjection, error) {
	t.Helper()
	input := make([]RunPlanStepInput, len(c.stepIDs))
	for i, id := range c.stepIDs {
		input[i] = RunPlanStepInput{StepID: id, Status: "completed"}
	}
	return c.store.SyncRunPlan(context.Background(), c.identity.TenantID, run.ID, "finish", input)
}

// The carry is a default for a step nobody looked at again. Once the Run checks
// the step itself, that check is the evidence: the saved plan stops claiming the
// older observation and stays editable. Without this, looking again at a carried
// step made every later update_plan fail on "already checked".
func TestACarriedStepYieldsToACheckThisRunMakesForIt(t *testing.T) {
	c := newCarryParent(t, carryShape{required: []bool{true, false}, completed: []int{0}, checked: []int{0}, active: 1})
	child := c.continueRun(t, c.run, true)
	c.recheck(t, child, 0, "recheck", 900)
	projection, err := c.finishAll(t, child)
	if err != nil {
		t.Fatalf("a re-verified carried step blocked the plan: %v", err)
	}
	if projection.Plan.Steps[0].ReusePriorVerification || projection.Plan.Steps[0].ReuseReason != "" {
		t.Fatalf("the saved plan still claims the older observation: %+v", projection.Plan.Steps[0])
	}
	if err := c.store.ValidateRunCompletion(context.Background(), c.identity.TenantID, child.ID); err != nil {
		t.Fatalf("the current check could not complete the Run: %v", err)
	}
}

// Between plan saves the stored plan still carries the flag; completion and the
// outcome verdict must read the current check, not fail on the stale carry.
func TestACarriedStepCheckedAfterTheLastPlanSaveDoesNotBlockCompletion(t *testing.T) {
	c := newCarryParent(t, carryShape{required: []bool{true, false}, completed: []int{0}, checked: []int{0}, active: 1})
	child := c.continueRun(t, c.run, true)
	if _, err := c.finishAll(t, child); err != nil {
		t.Fatal(err)
	}
	c.recheck(t, child, 0, "late-recheck", 900)
	if err := c.store.ValidateRunCompletion(context.Background(), c.identity.TenantID, child.ID); err != nil {
		t.Fatalf("a check made after the last plan save blocked completion: %v", err)
	}
	adopted, err := c.store.AdoptedRunVerificationChecks(context.Background(), c.identity.TenantID, child.ID)
	if err != nil {
		t.Fatalf("the outcome verdict failed on a carried step with a current check: %v", err)
	}
	if len(adopted) != 0 {
		t.Fatalf("the older observation was counted next to the current check: %+v", adopted)
	}
}

// A failed look at a carried step is not hidden by the carry.
func TestAFailedRecheckOfACarriedStepIsNotHiddenByTheCarry(t *testing.T) {
	c := newCarryParent(t, carryShape{required: []bool{true, false}, completed: []int{0}, checked: []int{0}, active: 1})
	child := c.continueRun(t, c.run, true)
	binding, err := c.store.ResolveVerificationBinding(context.Background(), c.identity.TenantID, child.ID, verification.Binding{StepID: c.stepIDs[0]}, "/workspace")
	if err != nil || binding == nil {
		t.Fatalf("binding: %+v %v", binding, err)
	}
	raw, _ := json.Marshal(map[string]interface{}{"evidence": map[string]interface{}{
		"kind": "verification", "tool_name": "verify", "tool_call_id": "failed-recheck", "status": "failed",
		"started_at_unix_nano": int64(900), "finished_at_unix_nano": int64(901), "invoked": true,
		"process": map[string]interface{}{"started": true, "exit_code": 1},
		"command": map[string]interface{}{"command": "check", "cwd": "/workspace", "exit_code": 1, "binding": binding},
	}})
	if _, err := c.store.AppendEvent(context.Background(), Event{RunID: child.ID, Type: "evidence.recorded", Payload: raw}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.finishAll(t, child); err == nil {
		t.Fatal("the carried observation completed a step whose current check failed")
	}
}

// When Main explicitly adopts a prior check and later runs a fresh check on the
// same step in the current Run, the current Run's check becomes authoritative
// instead of deadlocking subsequent update_plan calls.
func TestAnExplicitAdoptionStillConflictsWithACurrentCheck(t *testing.T) {
	c := newCarryParent(t, carryShape{required: []bool{true, false}, completed: []int{0}, checked: []int{0}, active: 1})
	child := c.continueRun(t, c.run, false) // a person's turn: Main decides
	if _, err := c.store.SyncRunPlan(context.Background(), c.identity.TenantID, child.ID, "adopt", []RunPlanStepInput{
		{StepID: c.stepIDs[0], Status: "completed", ReusePriorVerification: true, ReuseReason: "The same source still holds."},
		{StepID: c.stepIDs[1], Status: "in_progress"},
	}); err != nil {
		t.Fatalf("Main's adoption was refused: %v", err)
	}
	c.recheck(t, child, 0, "recheck", 900)
	projected, err := c.finishAll(t, child)
	if err != nil {
		t.Fatalf("current check after explicit adoption deadlocked update_plan: %v", err)
	}
	if projected.Plan.Steps[0].ReusePriorVerification || projected.Plan.Steps[0].ReuseReason != "" {
		t.Fatalf("current check did not supersede explicit adoption: %+v", projected.Plan.Steps[0])
	}
}

// RUQX-1272 recorded three successful builds and one failed one: the step that
// stands on the failed observation must not come back ticked.
func TestAStepWhoseCheckFailedIsNotCarriedAsDone(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	first, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "plan", []RunPlanStepInput{
		{Step: "Build image one", Status: "in_progress", SuccessCriteria: "Build one succeeded", VerificationRequired: true},
		{Step: "Build image two", Status: "pending", SuccessCriteria: "Build two succeeded", VerificationRequired: true},
		{Step: "Report", Status: "pending", SuccessCriteria: "Report sent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{first.Plan.Steps[0].StepID, first.Plan.Steps[1].StepID, first.Plan.Steps[2].StepID}
	for i, status := range []string{"succeeded", "failed"} {
		binding, err := store.ResolveVerificationBinding(ctx, identity.TenantID, run.ID, verification.Binding{StepID: ids[i], LocalDependencies: &[]string{}}, "/workspace")
		if err != nil || binding == nil {
			t.Fatalf("binding %d: %+v %v", i, binding, err)
		}
		exit := 0
		if status == "failed" {
			exit = 1
		}
		raw, _ := json.Marshal(map[string]interface{}{"evidence": map[string]interface{}{
			"kind": "verification", "tool_name": "verify", "tool_call_id": "build-" + status, "status": status,
			"started_at_unix_nano": int64(10 + 10*i), "finished_at_unix_nano": int64(11 + 10*i), "invoked": true,
			"process": map[string]interface{}{"started": true, "exit_code": exit},
			"command": map[string]interface{}{"command": "check build", "cwd": "/workspace", "exit_code": exit, "binding": binding},
		}})
		if _, err := store.AppendEvent(ctx, Event{RunID: run.ID, Type: "evidence.recorded", Payload: raw}); err != nil {
			t.Fatal(err)
		}
	}
	// Both builds are reported done while the report step stays open, so the work
	// unit has not closed and the plan accepts the snapshot.
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "progress", []RunPlanStepInput{
		{StepID: ids[0], Status: "completed"}, {StepID: ids[1], Status: "completed"}, {StepID: ids[2], Status: "in_progress"},
	}); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	c := carryRun{store: store, identity: identity, task: task, run: run, stepIDs: ids}
	child := c.continueRun(t, run, true)
	if got := c.planShape(t, child); got != "C.>" {
		t.Fatalf("the failed build came back ticked or the passed one was lost: [%s]", got)
	}
}

// When an earlier step was carried (or implicitly preserved on an unrelated step
// update) and the current Run mutates the workspace while working on a later step,
// updating that later step automatically reopens the stale carried step instead
// of rejecting update_plan.
func TestImplicitlyCarriedStepAutoReopensAfterWorkspaceMutationInCurrentRun(t *testing.T) {
	c := newCarryParent(t, carryShape{required: []bool{true, false, false}, completed: []int{0}, checked: []int{0}, active: 1})
	child := c.continueRun(t, c.run, true)
	if got := c.planShape(t, child); got != "C>." {
		t.Fatalf("expected initial carried shape C>., got [%s]", got)
	}
	// Mutate workspace during the child Run while working on step 1.
	c.recordMutation(t, child)
	// Patch only steps 1 and 2 via PatchRunPlan (which copies step 0 from previous.Steps).
	projected, err := c.store.PatchRunPlan(context.Background(), c.identity.TenantID, child.ID, 1, "advance step 1 to 2", []RunPlanStepInput{
		{StepID: c.stepIDs[1], Status: "completed"},
		{StepID: c.stepIDs[2], Status: "in_progress"},
	})
	if err != nil {
		t.Fatalf("unrelated step patch failed on stale carried step: %v", err)
	}
	if projected.Plan.Steps[0].Status != "pending" || projected.Plan.Steps[0].ReusePriorVerification {
		t.Fatalf("stale carried step was not auto-reopened: %+v", projected.Plan.Steps[0])
	}
	if len(projected.VerificationDeferred) != 1 || projected.VerificationDeferred[0].StepID != c.stepIDs[0] {
		t.Fatalf("expected step 0 in VerificationDeferred, got %+v", projected.VerificationDeferred)
	}
}

// When Main previously adopted an earlier step (or it was carried) and later
// mutates the workspace while completing the final step via a full snapshot
// update_plan, the stale adopted step is auto-reopened and promoted to
// in_progress so the plan stays valid instead of failing with a hard rejection.
func TestSnapshotModeAutoReopensPreviouslyAcceptedStepAndPromotesToInProgressWhenFinalStepCompletes(t *testing.T) {
	c := newCarryParent(t, carryShape{required: []bool{true, false}, completed: []int{0}, checked: []int{0}, active: 1})
	child := c.continueRun(t, c.run, false) // explicit adoption in child Run
	if _, err := c.store.SyncRunPlan(context.Background(), c.identity.TenantID, child.ID, "adopt step 0", []RunPlanStepInput{
		{StepID: c.stepIDs[0], Status: "completed", ReusePriorVerification: true, ReuseReason: "Parent check still holds."},
		{StepID: c.stepIDs[1], Status: "in_progress"},
	}); err != nil {
		t.Fatalf("initial adoption failed: %v", err)
	}
	// Mutate workspace while finishing step 1.
	c.recordMutation(t, child)
	// Submit full snapshot marking step 1 completed while echoing step 0 as completed.
	projected, err := c.store.SyncRunPlan(context.Background(), c.identity.TenantID, child.ID, "finish step 1", []RunPlanStepInput{
		{StepID: c.stepIDs[0], Status: "completed"},
		{StepID: c.stepIDs[1], Status: "completed"},
	})
	if err != nil {
		t.Fatalf("snapshot update_plan completing final step failed on stale adopted step: %v", err)
	}
	if projected.Plan.Steps[0].Status != "in_progress" || projected.Plan.Steps[0].ReusePriorVerification {
		t.Fatalf("expected reopened step 0 to be promoted to in_progress, got %+v", projected.Plan.Steps[0])
	}
	if projected.Plan.Steps[1].Status != "completed" {
		t.Fatalf("expected step 1 to remain completed, got %+v", projected.Plan.Steps[1])
	}
	if len(projected.VerificationDeferred) != 1 || projected.VerificationDeferred[0].StepID != c.stepIDs[0] {
		t.Fatalf("expected step 0 in VerificationDeferred, got %+v", projected.VerificationDeferred)
	}
}
