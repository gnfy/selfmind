package kernel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/syntax"
)

type RecoveryAttempt struct {
	PlanVersion           int
	PlanStepID            string
	ToolName              string
	InputSignature        string
	TargetHash            string
	Strategy              string
	EnvironmentGeneration int64
}

type RecoveryFailure struct {
	Attempt      RecoveryAttempt
	ErrorCode    string
	FailureClass string
	Retryability string
	EffectState  string
	StateChanged bool
	Alternatives []string
}

// RecoveryPolicy is injected so the Agent loop owns decisions while storage
// and orchestration remain outside kernel. It prevents a known-failed attempt
// before dispatch and learns only from typed tool results after dispatch.
type RecoveryPolicy interface {
	BeforeDispatch(RecoveryAttempt) error
	RecordFailure(RecoveryFailure)
	RecordSuccess(RecoveryAttempt)
}

type recoveryPolicyContextKey struct{}

func WithRecoveryPolicy(ctx context.Context, policy RecoveryPolicy) context.Context {
	if ctx == nil || policy == nil {
		return ctx
	}
	return context.WithValue(ctx, recoveryPolicyContextKey{}, policy)
}

func RecoveryPolicyFromContext(ctx context.Context) RecoveryPolicy {
	if ctx == nil {
		return nil
	}
	policy, _ := ctx.Value(recoveryPolicyContextKey{}).(RecoveryPolicy)
	return policy
}

type strategyRecoveryPolicy struct {
	mu       sync.Mutex
	failures map[string][]RecoveryFailure
}

func NewStrategyRecoveryPolicy() RecoveryPolicy {
	return &strategyRecoveryPolicy{failures: map[string][]RecoveryFailure{}}
}

func (p *strategyRecoveryPolicy) BeforeDispatch(attempt RecoveryAttempt) error {
	if p == nil || lifecycleTool(attempt.ToolName) {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	failures := p.failures[recoveryAttemptKey(attempt)]
	if len(failures) == 0 {
		return nil
	}
	last := failures[len(failures)-1]
	for _, failure := range failures {
		if failure.Attempt.InputSignature == attempt.InputSignature {
			return newRecoveryPolicyError(
				"recovery_attempt_repeated", "blocked_model_protocol", "different_strategy", "not_dispatched",
				"This exact failed tool attempt was already tried without new evidence or state.",
				appendRecoveryAlternatives(last.Alternatives, "inspect_current_state", "change_strategy", "report_actionable_blocker"),
			)
		}
	}
	if effectNeedsObservation(last.EffectState) && attempt.Strategy == "mutate" {
		return newRecoveryPolicyError(
			"unknown_effect_requires_observation", "uncertain_effect", "different_strategy", "not_dispatched",
			"A prior effect on this plan step has an unknown outcome. Observe current state before another mutation.",
			appendRecoveryAlternatives(last.Alternatives, "inspect_current_state", "verify_effect", "report_actionable_blocker"),
		)
	}
	if last.Retryability == "different_strategy" || len(failures) >= 2 {
		return newRecoveryPolicyError(
			"recovery_strategy_exhausted", "blocked_model_capability", "different_strategy", "not_dispatched",
			"This strategy already failed for the current plan step and target. A cosmetic argument change is not new progress.",
			appendRecoveryAlternatives(last.Alternatives, "inspect_current_state", "change_strategy", "report_actionable_blocker"),
		)
	}
	// One changed-input correction is allowed after new diagnostic evidence.
	return nil
}

func (p *strategyRecoveryPolicy) RecordFailure(failure RecoveryFailure) {
	if p == nil || lifecycleTool(failure.Attempt.ToolName) {
		return
	}
	// A guardrail or policy refusal never ran the tool, so it is not evidence
	// that the strategy fails against the target. Counting it consumed the one
	// correction the policy promises (observed live: a blocked preflight loop
	// plus one real failure locked out the corrected command).
	if strings.EqualFold(strings.TrimSpace(failure.EffectState), "not_dispatched") {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	key := recoveryAttemptKey(failure.Attempt)
	p.failures[key] = append(p.failures[key], failure)
}

func (p *strategyRecoveryPolicy) RecordSuccess(attempt RecoveryAttempt) {
	if p == nil {
		return
	}
	p.mu.Lock()
	for key, failures := range p.failures {
		if len(failures) == 0 {
			continue
		}
		failed := failures[len(failures)-1].Attempt
		sameStep := failed.PlanVersion == attempt.PlanVersion && failed.PlanStepID == attempt.PlanStepID &&
			failed.EnvironmentGeneration == attempt.EnvironmentGeneration
		if sameStep && (failed.TargetHash == attempt.TargetHash || attempt.Strategy == "mutate") {
			delete(p.failures, key)
		}
	}
	p.mu.Unlock()
}

func recoveryAttemptFromCall(ctx context.Context, toolName string, args map[string]interface{}, signature string, retryClass ToolRetryClass) RecoveryAttempt {
	planVersion, stepID := CurrentRunExecutionPlan(ctx)
	environmentGeneration := int64(0)
	if scope, ok := ToolInvocationScopeFromContext(ctx); ok {
		environmentGeneration = scope.EnvironmentGeneration
	}
	return RecoveryAttempt{
		PlanVersion: planVersion, PlanStepID: stepID, ToolName: strings.TrimSpace(toolName),
		InputSignature: strings.TrimSpace(signature), TargetHash: recoveryTargetHash(args),
		Strategy: ToolExecutionStrategy(toolName, retryClass), EnvironmentGeneration: environmentGeneration,
	}
}

func recoveryAttemptKey(attempt RecoveryAttempt) string {
	step := strings.TrimSpace(attempt.PlanStepID)
	if step == "" {
		step = "unplanned:" + strings.TrimSpace(attempt.ToolName)
	}
	return fmt.Sprintf("%d\x00%s\x00%s\x00%s\x00%d", attempt.PlanVersion, step,
		attempt.TargetHash, attempt.Strategy, attempt.EnvironmentGeneration)
}

func recoveryTargetHash(args map[string]interface{}) string {
	target := map[string]interface{}{}
	for key, value := range args {
		lower := strings.ToLower(strings.TrimSpace(key))
		if strings.HasPrefix(lower, "_") || recoveryCosmeticArg(lower) {
			continue
		}
		if recoveryTargetArg(lower) {
			target[lower] = value
		}
	}
	// Shell tools carry their target inside the command text. Without this,
	// every shell command in a plan step shared one key and two unrelated
	// failures exhausted the strategy for a corrected command.
	if command, ok := args["command"].(string); ok {
		if derived := shellCommandTarget(command); derived != "" {
			target["command_target"] = derived
		}
	}
	if len(target) == 0 {
		target["scope"] = "current"
	}
	data, _ := json.Marshal(target)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:12])
}

// shellCommandTarget names what a shell command acts on: the first external
// command plus up to two leading non-option words. Shell setup such as
// `cd repo &&` is skipped; otherwise every command launched from one directory
// would collapse to target "cd repo" and exhaust an unrelated strategy.
func shellCommandTarget(command string) string {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return shellCommandTargetFields(strings.Fields(command))
	}
	target := ""
	syntax.Walk(file, func(node syntax.Node) bool {
		if node == nil || target != "" {
			return false
		}
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		fields := make([]string, 0, len(call.Args))
		for _, word := range call.Args {
			value, ok := staticRecoveryShellWord(word)
			if !ok {
				continue
			}
			fields = append(fields, value)
		}
		if len(fields) == 0 || shellSetupCommand(fields[0]) {
			return true
		}
		target = shellCommandTargetFields(fields)
		return target == ""
	})
	return target
}

func shellCommandTargetFields(fields []string) string {
	words := make([]string, 0, 3)
	for _, field := range fields {
		if len(words) == 0 && isShellEnvAssignment(field) {
			continue
		}
		if strings.HasPrefix(field, "-") {
			continue
		}
		if len(words) == 0 {
			field = filepath.Base(field)
		}
		words = append(words, strings.ToLower(field))
		if len(words) == 3 {
			break
		}
	}
	return strings.Join(words, " ")
}

func shellSetupCommand(command string) bool {
	switch strings.ToLower(filepath.Base(strings.TrimSpace(command))) {
	case "cd", "export", "source", ".", "set", "unset", "umask":
		return true
	default:
		return false
	}
}

func staticRecoveryShellWord(word *syntax.Word) (string, bool) {
	if word == nil || len(word.Parts) == 0 {
		return "", false
	}
	var out strings.Builder
	for _, part := range word.Parts {
		switch value := part.(type) {
		case *syntax.Lit:
			out.WriteString(value.Value)
		case *syntax.SglQuoted:
			out.WriteString(value.Value)
		default:
			return "", false
		}
	}
	return out.String(), out.Len() > 0
}

func isShellEnvAssignment(field string) bool {
	name, _, found := strings.Cut(field, "=")
	if !found || name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

func recoveryTargetArg(key string) bool {
	for _, exact := range []string{"path", "file_path", "cwd", "workdir", "url", "uri", "project", "repository", "repo", "resource", "target", "query", "pattern"} {
		if key == exact {
			return true
		}
	}
	return strings.HasSuffix(key, "_id") || strings.HasSuffix(key, "_path") || strings.HasSuffix(key, "_url")
}

func recoveryCosmeticArg(key string) bool {
	switch key {
	case "timeout", "timeout_ms", "limit", "offset", "page", "max_results", "reason", "description":
		return true
	default:
		return false
	}
}

func lifecycleTool(name string) bool {
	switch strings.TrimSpace(name) {
	case "update_plan", "finish_run":
		return true
	default:
		return false
	}
}

func effectNeedsObservation(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "unknown", "uncertain", "dispatched":
		return true
	default:
		return false
	}
}

func appendRecoveryAlternatives(current []string, fallback ...string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(current)+len(fallback))
	for _, value := range append(append([]string(nil), current...), fallback...) {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

type recoveryPolicyError struct {
	code, category, retryability, effectState, message string
	alternatives                                       []string
}

func newRecoveryPolicyError(code, category, retryability, effectState, message string, alternatives []string) error {
	return &recoveryPolicyError{code: code, category: category, retryability: retryability,
		effectState: effectState, message: message, alternatives: alternatives}
}

func (e *recoveryPolicyError) Error() string             { return e.message }
func (e *recoveryPolicyError) ToolErrorCode() string     { return e.code }
func (e *recoveryPolicyError) ToolErrorCategory() string { return e.category }
func (e *recoveryPolicyError) ModelSafeMessage() string  { return e.message }
func (e *recoveryPolicyError) ToolRecoveryHint() string {
	return "Use the typed alternatives or finish with an actionable blocker; do not retry a cosmetic variant."
}
func (e *recoveryPolicyError) ToolFailurePhase() string { return "planning" }
func (e *recoveryPolicyError) ToolRetryability() string { return e.retryability }
func (e *recoveryPolicyError) ToolEffectState() string  { return e.effectState }
func (e *recoveryPolicyError) ToolStateChanged() bool   { return false }
func (e *recoveryPolicyError) ToolAlternatives() []string {
	return append([]string(nil), e.alternatives...)
}
