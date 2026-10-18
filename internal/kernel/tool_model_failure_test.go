package kernel

import (
	"errors"
	"strings"
	"testing"
)

func TestModelFailureKeepsActionableFactsWithoutRecoveryProtocol(t *testing.T) {
	err := stubRecoveryFailure{stubStableFailure{code: "watch_observation_unsupported", category: "capability_unavailable", safe: "This observation cannot be registered.", hint: "Use a single read-only observation instead."}}
	env := packageToolError("watch_external", err)
	for _, forbidden := range []string{"error_code:", "error_class:", "failure_phase:", "retryability:", "effect_state:", "alternatives:", "SelfMind diagnostic instruction"} {
		if strings.Contains(env.ModelContent, forbidden) {
			t.Errorf("model still has to read recovery protocol %q: %s", forbidden, env.ModelContent)
		}
	}
	for _, fact := range []string{"Not executed", "This observation cannot be registered.", "Use a single read-only observation instead."} {
		if !strings.Contains(env.ModelContent, fact) {
			t.Errorf("missing actionable fact %q: %s", fact, env.ModelContent)
		}
	}
	if env.ErrorCode != "watch_observation_unsupported" || env.FailurePhase != "preparation" || env.EffectState != "not_dispatched" || len(env.Alternatives) != 2 || !strings.Contains(env.Raw, "error_code:") {
		t.Fatalf("simplifying model text erased internal recovery facts: %+v", env)
	}
}

func TestFailureWithCapturedOutputDoesNotPrescribeAnotherAttempt(t *testing.T) {
	env := packageToolFailureCtx(nil, "verify", "Observed value: false", errors.New("command failed: exit status 1"))
	if !strings.Contains(env.ModelContent, "Observed value: false") || !strings.Contains(env.ModelContent, "exit status 1") {
		t.Fatal("failure lost observation or execution result")
	}
	for _, forbidden := range []string{"SelfMind diagnostic instruction", "Correct the next step", "error_class:"} {
		if strings.Contains(env.ModelContent, forbidden) {
			t.Fatalf("failure prescribed recovery instead of exposing facts: %s", env.ModelContent)
		}
	}
}

func TestSimplifiedFailurePreservesSafetyAndCapturedData(t *testing.T) {
	for _, test := range []struct {
		name       string
		err        error
		constraint string
	}{
		{"rejection", errors.New("operation rejected: " + strings.Repeat("detail ", 900)), "Do NOT retry"},
		{"uncertain effect", &toolOutcomePersistenceError{cause: errors.New("database is locked")}, "Observe their state before retrying"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Protocol-looking text in command output is evidence, not framework
			// metadata. Never filter it while simplifying the error description.
			const output = "error_class: a literal line in the file\nresult: partial"
			env := packageToolFailureCtx(nil, "terminal", output, test.err)
			if !strings.HasPrefix(env.ModelContent, "This operation was declined.") && test.name == "rejection" {
				t.Fatal("the decision was buried behind bounded details")
			}
			if !strings.Contains(env.ModelContent, test.constraint) || !strings.Contains(env.ModelContent, output) {
				t.Fatalf("lost safety or observed data: %s", env.ModelContent)
			}
			if test.name == "uncertain effect" && (env.EffectState != "unknown" || env.FailurePhase != "outcome_recording") {
				t.Fatal("uncertainty became a preparation refusal")
			}
		})
	}
}

func TestModelFailureKeepsExternalErrorFieldsAsEvidence(t *testing.T) {
	const detail = "External database request failed\nerror_code: E112\neffect_state: transaction still open"
	env := packageToolErrorWithMetadata("query", errors.New(detail), ToolExecutionMetadata{Origin: "external"})
	if !strings.Contains(env.ModelContent, detail) {
		t.Fatalf("external error fields were mistaken for runtime metadata: %s", env.ModelContent)
	}
}
