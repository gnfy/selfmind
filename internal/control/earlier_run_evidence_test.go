package control

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"selfmind/internal/verification"
)

// appendObservedCheck records a check as the real tool does: invoked, with a
// confirmed process and exit, so it can back an assessment.
func appendObservedCheck(t *testing.T, store *Store, runID, id string, binding *verification.Binding, at int64) {
	t.Helper()
	raw, _ := json.Marshal(map[string]interface{}{"evidence": map[string]interface{}{
		"kind": "verification", "tool_name": "verify", "tool_call_id": id, "status": "succeeded",
		"started_at_unix_nano": at, "finished_at_unix_nano": at + 1, "invoked": true,
		"process": map[string]interface{}{"started": true, "exit_code": 0},
		"command": map[string]interface{}{"command": "check", "cwd": "/workspace", "exit_code": 0, "binding": binding},
	}})
	if _, err := store.AppendEvent(context.Background(), Event{RunID: runID, Type: "evidence.recorded", Payload: raw}); err != nil {
		t.Fatal(err)
	}
}

func citeEarlierRun(t *testing.T, c carryRun, run *Run, ref string) error {
	t.Helper()
	_, err := c.store.ResolveConditionAssessments(context.Background(), c.identity.TenantID, run.ID,
		[]verification.Assessment{{Result: "met", EvidenceRefs: []string{ref}, Reason: "Observed in the previous Run."}})
	if err == nil {
		t.Fatalf("an observation from an earlier Run was accepted by its bare id")
	}
	var input interface{ ConditionAssessmentInput() bool }
	if !errors.As(err, &input) {
		t.Fatalf("the refusal is not a correctable input error: %v", err)
	}
	return err
}

func requireText(t *testing.T, err error, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal lacks %q:\n%v", want, err)
		}
	}
}

// RUQX-1272: finish_run cited the previous Run's check id and was refused with
// only "unavailable". The refusal now says what the id is and what to do.
func TestAssessmentCitingAnAdoptedObservationSaysItNeedsNoAssessment(t *testing.T) {
	c := newCarryParent(t, carryShape{required: []bool{true, false}, completed: []int{0}, checked: []int{0}, active: 1})
	child := c.continueRun(t, c.run, true)
	err := citeEarlierRun(t, c, child, "check-0")
	requireText(t, err, `"check-0"`, "already stands on", c.stepIDs[0])
	if strings.Contains(err.Error(), "has no effective verify check yet") {
		t.Fatalf("generic advice contradicts the note:\n%v", err)
	}
}

func TestAssessmentCitingAnObservationThatCanBeAdoptedOffersTheAdoption(t *testing.T) {
	c := newCarryParent(t, carryShape{required: []bool{true, false}, completed: []int{0}, checked: []int{0}, active: 1})
	child := c.continueRun(t, c.run, false) // a person's turn: the step reopened
	err := citeEarlierRun(t, c, child, "check-0")
	requireText(t, err, "earlier-Run observation behind step "+c.stepIDs[0], "reuse_prior_verification=true", "verify with step_id="+c.stepIDs[0])
}

func TestAssessmentCitingAnObservationThatCannotBeAdoptedSaysWhy(t *testing.T) {
	c := newCarryParent(t, carryShape{required: []bool{true, false}, completed: []int{0}, checked: []int{0}, active: 1})
	c.recordMutation(t, c.run) // after the check, in the run that made it
	child := c.continueRun(t, c.run, false)
	err := citeEarlierRun(t, c, child, "check-0")
	requireText(t, err, "cannot adopt it now", "inputs changed", "verify with step_id="+c.stepIDs[0])
}

// The list names what can be cited: this Run's own effective checks.
func TestAssessmentRefusalListsTheChecksOfThisRun(t *testing.T) {
	c := newCarryParent(t, carryShape{required: []bool{true, false}, completed: []int{0}, checked: []int{0}, active: 1})
	child := c.continueRun(t, c.run, true)
	binding, err := c.store.ResolveVerificationBinding(context.Background(), c.identity.TenantID, child.ID, verification.Binding{StepID: c.stepIDs[1]}, "/workspace")
	if err != nil || binding == nil {
		t.Fatalf("binding: %+v %v", binding, err)
	}
	appendObservedCheck(t, c.store, child.ID, "fresh-check", binding, 900)
	refusal := citeEarlierRun(t, c, child, "check-0")
	requireText(t, refusal, "Citeable now: fresh-check (step "+c.stepIDs[1]+", target", "already stands on")
	// The id of this Run's own check is still accepted.
	if _, err := c.store.ResolveConditionAssessments(context.Background(), c.identity.TenantID, child.ID,
		[]verification.Assessment{{Result: "met", EvidenceRefs: []string{"fresh-check"}, Reason: "Observed now."}}); err != nil {
		t.Fatalf("this Run's own check was refused: %v", err)
	}
}

// An id that is nowhere in the Run's evidence still gets the list, with no note.
func TestAssessmentRefusalForAnUnknownIDHasNoEarlierRunNote(t *testing.T) {
	c := newCarryParent(t, carryShape{required: []bool{true, false}, completed: []int{0}, checked: []int{0}, active: 1})
	child := c.continueRun(t, c.run, true)
	err := citeEarlierRun(t, c, child, "toolcall-99-0")
	if strings.Contains(err.Error(), "earlier-Run observation") {
		t.Fatalf("a note was invented for an id the Run does not know: %v", err)
	}
	requireText(t, err, `"toolcall-99-0"`, "call ids restart in every Run")
}

func TestReplacesNamingAnEarlierRunCheckPointsAtTheStep(t *testing.T) {
	c := newCarryParent(t, carryShape{required: []bool{true, false}, completed: []int{0}, checked: []int{0}, active: 1})
	child := c.continueRun(t, c.run, false)
	_, err := c.store.ResolveVerificationBinding(context.Background(), c.identity.TenantID, child.ID, verification.Binding{Replaces: "check-0", Reason: "retry"}, "/workspace")
	if err == nil {
		t.Fatal("a replacement of another Run's check was accepted")
	}
	requireText(t, err, "replaces names a check made in this Run", "earlier-Run observation behind step "+c.stepIDs[0], "verify with step_id="+c.stepIDs[0])

	_, err = c.store.ResolveVerificationBinding(context.Background(), c.identity.TenantID, child.ID, verification.Binding{Replaces: "toolcall-99-0", Reason: "retry"}, "/workspace")
	if err == nil {
		t.Fatal("a replacement of an unknown check was accepted")
	}
	requireText(t, err, "bind verify to its step_id")
}
