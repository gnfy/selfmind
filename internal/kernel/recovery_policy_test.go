package kernel

import (
	"context"
	"errors"
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
)

func TestStrategyRecoveryPolicyBlocksRepeatedAndCosmeticRetries(t *testing.T) {
	policy := NewStrategyRecoveryPolicy()
	base := RecoveryAttempt{
		PlanVersion: 2, PlanStepID: "step-a", ToolName: "terminal",
		InputSignature: "terminal\x00one", TargetHash: "workspace", Strategy: "mutate", EnvironmentGeneration: 3,
	}
	if err := policy.BeforeDispatch(base); err != nil {
		t.Fatal(err)
	}
	policy.RecordFailure(RecoveryFailure{Attempt: base, FailureClass: "exit_status"})
	if err := policy.BeforeDispatch(base); err == nil || !recoveryErrorCode(err, "recovery_attempt_repeated") {
		t.Fatalf("exact retry error=%T %v", err, err)
	}

	corrected := base
	corrected.InputSignature = "terminal\x00two"
	if err := policy.BeforeDispatch(corrected); err != nil {
		t.Fatalf("one evidence-backed correction was refused: %v", err)
	}
	policy.RecordFailure(RecoveryFailure{Attempt: corrected, FailureClass: "exit_status"})
	cosmetic := base
	cosmetic.InputSignature = "terminal\x00three"
	if err := policy.BeforeDispatch(cosmetic); err == nil || !recoveryErrorCode(err, "recovery_strategy_exhausted") {
		t.Fatalf("third same-strategy attempt error=%T %v", err, err)
	}

	different := base
	different.Strategy = "observe"
	different.InputSignature = "read_file\x00state"
	if err := policy.BeforeDispatch(different); err != nil {
		t.Fatalf("genuinely different strategy was refused: %v", err)
	}
	policy.RecordSuccess(base)
	if err := policy.BeforeDispatch(base); err != nil {
		t.Fatalf("new successful evidence did not release correction budget: %v", err)
	}
}

// Shell commands carry their target in the command text. Two aws subcommands
// are different targets, while an env prefix or an extra flag is the same
// target — so a corrected command is a correction and a re-flagged one is a
// cosmetic retry.
func TestRecoveryTargetHashDerivesShellCommandTarget(t *testing.T) {
	listBuilds := recoveryTargetHash(map[string]interface{}{
		"command": `ids=$(aws codebuild list-builds-for-project --project-name api --query ids --output text)`,
	})
	listBuildsEnv := recoveryTargetHash(map[string]interface{}{
		"command": `PROFILE=cw2 aws codebuild list-builds-for-project --sort-order DESCENDING --project-name api`,
	})
	batchGet := recoveryTargetHash(map[string]interface{}{
		"command": `aws codebuild batch-get-builds --ids build-1`,
	})
	if listBuilds != listBuildsEnv {
		t.Fatalf("env prefix and flag order must not change the target: %s vs %s", listBuilds, listBuildsEnv)
	}
	if listBuilds == batchGet {
		t.Fatal("different subcommands must be different targets")
	}
	if got := shellCommandTarget(`cd /tmp && npm test`); got != "npm test" {
		t.Fatalf("shell setup must not become the recovery target: %q", got)
	}
}

// A guardrail or policy refusal never executed the tool. It must not consume
// the one changed-input correction the policy promises (observed live: one
// blocked preflight loop plus one real failure locked out the corrected
// command as "strategy exhausted").
func TestStrategyRecoveryPolicyIgnoresNotDispatchedRefusals(t *testing.T) {
	policy := NewStrategyRecoveryPolicy()
	blocked := RecoveryAttempt{
		PlanVersion: 1, PlanStepID: "step-a", ToolName: "terminal",
		InputSignature: "terminal\x00loop", TargetHash: "aws codebuild batch-get-builds", Strategy: "observe",
	}
	policy.RecordFailure(RecoveryFailure{Attempt: blocked, ErrorCode: "active_turn_polling", EffectState: "not_dispatched"})
	real := blocked
	real.InputSignature = "terminal\x00list-builds"
	if err := policy.BeforeDispatch(real); err != nil {
		t.Fatalf("refusal must not count against the target: %v", err)
	}
	policy.RecordFailure(RecoveryFailure{Attempt: real, FailureClass: "command_failed"})
	corrected := real
	corrected.InputSignature = "terminal\x00list-builds-fixed"
	if err := policy.BeforeDispatch(corrected); err != nil {
		t.Fatalf("the corrected command after one real failure was refused: %v", err)
	}
	policy.RecordFailure(RecoveryFailure{Attempt: corrected, FailureClass: "command_failed"})
	third := real
	third.InputSignature = "terminal\x00list-builds-third"
	if err := policy.BeforeDispatch(third); err == nil || !recoveryErrorCode(err, "recovery_strategy_exhausted") {
		t.Fatalf("two real failures on one target must exhaust the strategy: %v", err)
	}
}

func TestStrategyRecoveryPolicyReleasesFailureAfterStateChange(t *testing.T) {
	policy := NewStrategyRecoveryPolicy()
	observe := RecoveryAttempt{PlanVersion: 1, PlanStepID: "step-a", ToolName: "read_file", InputSignature: "read", TargetHash: "file", Strategy: "observe"}
	policy.RecordFailure(RecoveryFailure{Attempt: observe, FailureClass: "not_found"})
	mutation := RecoveryAttempt{PlanVersion: 1, PlanStepID: "step-a", ToolName: "write_file", InputSignature: "write", TargetHash: "other-shape", Strategy: "mutate"}
	policy.RecordSuccess(mutation)
	if err := policy.BeforeDispatch(observe); err != nil {
		t.Fatalf("successful state change did not release failed observation: %v", err)
	}
}

func TestStrategyRecoveryPolicyRequiresObservationForUnknownEffect(t *testing.T) {
	policy := NewStrategyRecoveryPolicy()
	mutation := RecoveryAttempt{PlanStepID: "step-a", ToolName: "terminal", InputSignature: "first", TargetHash: "target", Strategy: "mutate"}
	policy.RecordFailure(RecoveryFailure{Attempt: mutation, EffectState: "unknown", Retryability: "different_strategy", Alternatives: []string{"one_shot_observation"}})
	nextMutation := mutation
	nextMutation.InputSignature = "second"
	if err := policy.BeforeDispatch(nextMutation); err == nil || !recoveryErrorCode(err, "unknown_effect_requires_observation") {
		t.Fatalf("unknown effect mutation error=%T %v", err, err)
	}
	observe := mutation
	observe.Strategy = "observe"
	observe.ToolName = "read_file"
	observe.InputSignature = "inspect"
	if err := policy.BeforeDispatch(observe); err != nil {
		t.Fatalf("observation strategy was refused: %v", err)
	}
}

func recoveryErrorCode(err error, want string) bool {
	var stable interface{ ToolErrorCode() string }
	return errors.As(err, &stable) && stable.ToolErrorCode() == want
}

type countingFailureBackend struct{ calls int }

func (b *countingFailureBackend) Dispatch(string, map[string]interface{}) (string, error) {
	b.calls++
	return "", errors.New("command failed with exit status 1")
}

func (*countingFailureBackend) GetToolDefinitions() []map[string]interface{} { return nil }

type verificationBackend struct{ calls []string }

func (b *verificationBackend) Dispatch(name string, _ map[string]interface{}) (string, error) {
	b.calls = append(b.calls, name)
	return "ok", nil
}

func (*verificationBackend) GetToolDefinitions() []map[string]interface{} {
	definition := func(name string, readOnly bool) map[string]interface{} {
		return map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name": name, "description": name,
				"parameters": map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
			},
			"selfmind": map[string]interface{}{"read_only": readOnly},
		}
	}
	return []map[string]interface{}{definition("inspect", true), definition("mutate", false), definition("finish_run", false)}
}

func (*verificationBackend) ToolExecutionMetadata(name string, _ map[string]interface{}) ToolExecutionMetadata {
	return ToolExecutionMetadata{ReadOnly: name == "inspect"}
}

func TestVerificationOnlyRecoveryHidesAndRefusesMutation(t *testing.T) {
	backend := &verificationBackend{}
	agent := &Agent{backend: backend}
	strategy := DefaultTaskStrategy()
	strategy.VerificationOnly = true
	defs := agent.llmToolDefinitions(context.Background(), strategy)
	if len(defs) != 2 || defs[0].Name != "inspect" || defs[1].Name != "finish_run" {
		t.Fatalf("verification-only definitions=%+v", defs)
	}
	ctx := WithToolInvocationScope(context.Background(), ToolInvocationScope{RunID: "run-recovery", RecoveryMode: "verify_only"})
	refused := agent.executeSingleToolCall(ctx, "default", nil, 0, llm.ToolCall{ID: "mutate-1", Function: "mutate", Args: `{}`})
	if refused.success || len(backend.calls) != 0 || !strings.Contains(refused.msg.Content, "error_code: verification_only_mutation_refused") {
		t.Fatalf("mutation escaped verification-only recovery: result=%+v calls=%v", refused, backend.calls)
	}
	observed := agent.executeSingleToolCall(ctx, "default", nil, 0, llm.ToolCall{ID: "read-1", Function: "inspect", Args: `{}`})
	if !observed.success || len(backend.calls) != 1 || backend.calls[0] != "inspect" {
		t.Fatalf("read-only observation was refused: result=%+v calls=%v", observed, backend.calls)
	}
}

func TestAgentRefusesRepeatedFailedDispatchBeforeToolExecution(t *testing.T) {
	backend := &countingFailureBackend{}
	agent := &Agent{backend: backend}
	ctx := WithRecoveryPolicy(context.Background(), NewStrategyRecoveryPolicy())
	ctx = WithRunExecutionState(ctx, NewRunExecutionState())
	UpdateRunExecutionPlan(ctx, 1, "step-a")
	first := agent.executeSingleToolCall(ctx, "default", nil, 0, llm.ToolCall{ID: "call-1", Function: "terminal", Args: `{"command":"false"}`})
	if first.success || backend.calls != 1 {
		t.Fatalf("first failure=%+v calls=%d", first, backend.calls)
	}
	second := agent.executeSingleToolCall(ctx, "default", nil, 0, llm.ToolCall{ID: "call-2", Function: "terminal", Args: `{"command":"false"}`})
	if second.success || backend.calls != 1 || !strings.Contains(second.msg.Content, "error_code: recovery_attempt_repeated") ||
		!strings.Contains(second.msg.Content, "effect_state: not_dispatched") {
		t.Fatalf("repeat was dispatched or lost typed recovery: calls=%d result=%+v", backend.calls, second)
	}
}
