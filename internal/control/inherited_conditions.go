package control

import (
	"context"
	"database/sql"

	"selfmind/internal/verification"
)

// Historical Main findings travel with the exact observation they interpreted.
// They are context only: adoption and current completion still validate their
// own scope, freshness and positive verification requirements.
func attachInheritedConditionsTx(ctx context.Context, tx *sql.Tx, items []InheritedPlanEvidence) error {
	byRun := map[string][]verification.Assessment{}
	for i := range items {
		item := &items[i]
		item.PriorCondition = nil
		if item.CheckID == "" {
			continue
		}
		assessments, loaded := byRun[item.SourceRunID]
		if !loaded {
			var err error
			assessments, err = recordedConditionAssessmentsTx(ctx, tx, item.SourceRunID)
			if err != nil {
				return err
			}
			byRun[item.SourceRunID] = assessments
		}
		for _, assessment := range assessments {
			if assessment.StepID != item.SourceStepID || assessment.PlanCriterion != item.SourceCriterion ||
				assessment.Criterion != item.ObservedCriterion || assessment.Target != item.Target {
				continue
			}
			for _, ref := range assessment.EvidenceRefs {
				if ref == item.CheckID {
					copy := assessment
					item.PriorCondition = &copy
					break
				}
			}
		}
	}
	return nil
}

func sourceConditionBlockTx(ctx context.Context, tx *sql.Tx, runID, stepID, criterion string) (string, error) {
	assessments, err := recordedConditionAssessmentsTx(ctx, tx, runID)
	if err != nil {
		return "", err
	}
	for _, assessment := range assessments {
		if assessment.StepID == stepID && assessment.PlanCriterion == criterion && assessment.Result != "met" {
			return "Main's recorded condition finding is " + assessment.Result + "; a successful process exit is not a positive condition verdict", nil
		}
	}
	return "", nil
}
