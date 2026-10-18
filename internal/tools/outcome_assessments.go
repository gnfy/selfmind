package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"selfmind/internal/verification"
)

type conditionAssessmentProjection interface {
	PrepareConditionAssessments(context.Context, string, []verification.Assessment) ([]verification.Assessment, error)
}

func conditionAssessmentsProperty() PropertyDef {
	return PropertyDef{Type: "array", Description: "Main's condition findings, separate from exit status; include negative/uncertain results. Prefer step_id: runtime binds current or explicitly adopted proof. Otherwise cite current-Run evidence_refs, never historical ids.", Items: &PropertyDef{
		Type: "object",
		Properties: map[string]PropertyDef{
			"result":        {Type: "string", Enum: []string{"met", "not_met", "unknown"}, Description: "Whether the condition holds. A negative finding may complete inspection, never required positive acceptance."},
			"step_id":       {Type: "string", Description: "Current Plan step for one condition and target; binds current or adopted step proof."},
			"evidence_refs": {Type: "array", Items: &PropertyDef{Type: "string"}, Description: "Optional 1–8 current-Run verification or observation references. To associate ordinary tool observations, supply step_id too and finish with plan_patch. Static reads cannot prove executable verification."},
			"reason":        {Type: "string", Description: "Why the observations support this finding (at most 1000 bytes)."},
		}, Required: []string{"result", "reason"},
	}}
}

func prepareOutcomeAssessments(args map[string]interface{}, status string, projection RunPlanProjection) ([]verification.Assessment, bool, error) {
	raw, supplied := args["assessments"]
	if !supplied {
		return nil, false, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, false, err
	}
	var input []verification.Assessment
	if err := json.Unmarshal(encoded, &input); err != nil || len(input) == 0 || len(input) > 16 {
		return nil, false, finishInputError(fmt.Errorf("assessments must contain 1–16 condition judgments"), "condition_assessment_invalid", "Supply result, reason and either step_id or current-Run evidence_refs for each finding, then send finish_run again.")
	}
	resolver, ok := projection.(conditionAssessmentProjection)
	if !ok {
		return nil, false, fmt.Errorf("condition assessments require the durable Run evidence resolver")
	}
	conditions, err := resolver.PrepareConditionAssessments(ContextFromArgs(args), status, input)
	return conditions, true, err
}
