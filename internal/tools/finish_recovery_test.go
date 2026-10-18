package tools

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"selfmind/internal/verification"
)

// assessmentInputProjection plays the durable evidence resolver: it refuses
// the judgments it is given with whatever error the test chooses.
type assessmentInputProjection struct {
	durablePlanProjectionTest
	err error
}

func (p *assessmentInputProjection) PrepareConditionAssessments(context.Context, string, []verification.Assessment) ([]verification.Assessment, error) {
	return nil, p.err
}

type conditionInputTestError struct{ message string }

func (e conditionInputTestError) Error() string                { return e.message }
func (conditionInputTestError) ConditionAssessmentInput() bool { return true }

type recoveryContract interface {
	ToolErrorCode() string
	ToolFailurePhase() string
	ToolRetryability() string
	ToolEffectState() string
}

// finish_run is capped at one call per Run, and the kernel hands that call back
// once only when the refusal says nothing was dispatched and the input can be
// corrected. A finish refusal that lacks the contract costs the Run its only way
// to close (RUQX-1272: an unavailable evidence reference, then two hidden retries,
// then an interrupted Run).
func TestFinishInputRefusalsCarryTheContractThatReturnsTheAttempt(t *testing.T) {
	missing := conditionInputTestError{`verification evidence "toolcall-15-0" is unavailable or no longer effective in this Run`}
	ctx := WithRunPlanProjection(context.Background(), &assessmentInputProjection{err: missing})
	judgment := []interface{}{map[string]interface{}{"result": "met", "evidence_refs": []interface{}{"toolcall-15-0"}, "reason": "The source was read."}}
	cases := []struct {
		name string
		args map[string]interface{}
		code string
	}{
		{"missing status", map[string]interface{}{"summary": "done"}, "tool_arguments_invalid"},
		{"unknown status", map[string]interface{}{"status": "finished", "summary": "done"}, "tool_arguments_invalid"},
		{"no body", map[string]interface{}{"status": "done"}, "tool_arguments_invalid"},
		{"empty assessments", map[string]interface{}{"status": "done", "final_response": "ok", "assessments": []interface{}{}, "_context": ctx}, "condition_assessment_invalid"},
		{"unavailable evidence", map[string]interface{}{"status": "done", "final_response": "ok", "assessments": judgment, "_context": ctx}, "condition_assessment_invalid"},
	}
	for _, tc := range cases {
		_, err := NewFinishRunTool().Execute(tc.args)
		var contract recoveryContract
		if !errors.As(err, &contract) {
			t.Fatalf("%s: refusal has no recovery contract: %v", tc.name, err)
		}
		if contract.ToolErrorCode() != tc.code || contract.ToolFailurePhase() != "preparation" ||
			contract.ToolRetryability() != "corrected_input" || contract.ToolEffectState() != "not_dispatched" {
			t.Fatalf("%s: contract = %s/%s/%s/%s", tc.name, contract.ToolErrorCode(), contract.ToolFailurePhase(), contract.ToolRetryability(), contract.ToolEffectState())
		}
	}
}

// A storage failure, an unmet completion requirement and a rejection each keep
// their own meaning: only an input mistake the model can correct is handed back.
func TestFinishRefusalsThatAreNotInputMistakesKeepTheirOwnMeaning(t *testing.T) {
	storage := &assessmentInputProjection{err: fmt.Errorf("read condition history: disk I/O error")}
	_, err := NewFinishRunTool().Execute(map[string]interface{}{
		"status": "done", "final_response": "ok", "_context": WithRunPlanProjection(context.Background(), storage),
		"assessments": []interface{}{map[string]interface{}{"result": "met", "evidence_refs": []interface{}{"x"}, "reason": "r"}},
	})
	var contract recoveryContract
	if err == nil || (errors.As(err, &contract) && contract.ToolRetryability() == "corrected_input") {
		t.Fatalf("a storage failure was presented as a correctable input: %v", err)
	}
	unmet := &durablePlanProjectionTest{completionErr: completionPreconditionTestError{}}
	_, err = NewFinishRunTool().Execute(map[string]interface{}{"status": "done", "final_response": "ok", "_context": WithRunPlanProjection(context.Background(), unmet)})
	if !errors.As(err, &contract) || contract.ToolErrorCode() != "completion_precondition" || contract.ToolRetryability() != "after_state_change" {
		t.Fatalf("an unmet completion requirement lost its own meaning: %v", err)
	}
}

type completionPreconditionTestError struct{}

func (completionPreconditionTestError) Error() string                { return "unresolved evidence" }
func (completionPreconditionTestError) CompletionPrecondition() bool { return true }
