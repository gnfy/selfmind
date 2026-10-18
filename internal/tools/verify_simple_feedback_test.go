package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"selfmind/internal/kernel"
)

func TestFailedCheckDoesNotChooseMainPlanOrRetry(t *testing.T) {
	ctx := WithRunPlanProjection(context.Background(), evidenceVerificationProjection{})
	exec := EvidenceMiddleware()(func(map[string]interface{}) (kernel.ToolDispatchResult, error) {
		code := 1
		return kernel.ToolDispatchResult{Output: "Observed value: false", Process: &kernel.ToolProcessResult{Started: true, ExitCode: &code}}, errors.New("check exited 1")
	})
	result, err := exec(map[string]interface{}{"_context": ctx, "_tool_name": "verify", "_tool_call_id": "check-negative", "command": "check condition", "criterion": "condition holds", "target": "artifact"})
	if err == nil || len(result.Evidence) != 1 || result.Evidence[0].Status != "failed" {
		t.Fatal("negative observation was weakened")
	}
	for _, forbidden := range []string{"correct this same check", "keep diagnostic and report steps pending", "in_progress", "Cancel any"} {
		if strings.Contains(result.Output, forbidden) {
			t.Errorf("runtime chose recovery for Main: %s", result.Output)
		}
	}
	if !strings.Contains(result.Output, "check-negative") {
		t.Fatal("lost addressable evidence")
	}
}

func TestIndependentVerificationNamesMissingTarget(t *testing.T) {
	_, err := prepareVerificationBinding(map[string]interface{}{"criterion": "condition holds"})
	if err == nil || !strings.Contains(err.Error(), "target is required") || strings.Contains(err.Error(), "nonempty criterion and target") {
		t.Fatalf("missing field was not actionable: %v", err)
	}
}
