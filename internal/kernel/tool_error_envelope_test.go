package kernel

import (
	"errors"
	"strings"
	"testing"
)

// stubStableFailure is a tool error that describes itself, i.e. the opt-in path.
type stubStableFailure struct {
	code, category, safe, hint string
}

func (e stubStableFailure) Error() string             { return "resolve work unit: sql: no rows in result set" }
func (e stubStableFailure) ToolErrorCode() string     { return e.code }
func (e stubStableFailure) ToolErrorCategory() string { return e.category }
func (e stubStableFailure) ModelSafeMessage() string  { return e.safe }
func (e stubStableFailure) ToolRecoveryHint() string  { return e.hint }

type stubRecoveryFailure struct{ stubStableFailure }

func (stubRecoveryFailure) ToolFailurePhase() string { return "preparation" }
func (stubRecoveryFailure) ToolRetryability() string { return "different_strategy" }
func (stubRecoveryFailure) ToolEffectState() string  { return "not_dispatched" }
func (stubRecoveryFailure) ToolStateChanged() bool   { return false }
func (stubRecoveryFailure) ToolAlternatives() []string {
	return []string{"one_shot_observation", "provider_native_wait"}
}

func TestToolErrorEnvelopeCarriesRecoveryContract(t *testing.T) {
	err := stubRecoveryFailure{stubStableFailure{
		code:     "watch_observation_unsupported",
		category: "capability_unavailable", safe: "The durable watcher cannot prove this command read-only.",
		hint: "Choose another observation strategy.",
	}}
	env := packageToolError("watch_external", err)
	if env.FailurePhase != "preparation" || env.Retryability != "different_strategy" || env.EffectState != "not_dispatched" || env.StateChanged {
		t.Fatalf("recovery envelope = %+v", env)
	}
	if len(env.Alternatives) != 2 || !strings.Contains(env.ModelContent, "provider_native_wait") {
		t.Fatalf("recovery alternatives missing from model content: %+v", env)
	}
}

// TestUnwrappedStorageErrorIsRedactedAtTheBoundary pins A-13: the typed envelope
// is opt-in per call site, so an unwrapped internal-storage error used to reach
// the model verbatim. The raw cause must survive in the capture surface.
func TestUnwrappedStorageErrorIsRedactedAtTheBoundary(t *testing.T) {
	for _, raw := range []string{
		"resolve work unit: sql: no rows in result set",
		"search failed: SQL logic error: no such column: 625 (1)",
		"write failed: constraint failed",
		"open failed: database is locked",
	} {
		env := packageToolError("skill_select", errors.New(raw))
		if strings.Contains(env.ModelContent, "sql:") || strings.Contains(strings.ToLower(env.ModelContent), "sql logic error") ||
			strings.Contains(env.ModelContent, "constraint failed") || strings.Contains(env.ModelContent, "database is locked") {
			t.Errorf("raw storage text reached the model surface for %q: %q", raw, env.ModelContent)
		}
		if env.ErrorCategory != "internal_state" {
			t.Errorf("category for %q = %q, want internal_state", raw, env.ErrorCategory)
		}
		if env.DiagnosticExcerpt == "" || !strings.Contains(env.DiagnosticExcerpt, raw) {
			t.Errorf("raw cause for %q must stay in the diagnostic capture, got %q", raw, env.DiagnosticExcerpt)
		}
		if env.DiagnosticHash == "" {
			t.Errorf("capture for %q has no hash to reference", raw)
		}
	}
}

// TestOrdinaryToolErrorsAreNotRedacted guards against the leak guard turning
// into a blanket redactor: real, actionable failures must reach the model.
func TestOrdinaryToolErrorsAreNotRedacted(t *testing.T) {
	for _, raw := range []string{
		"command failed: exit status 1",
		"command timed out after 60 seconds",
		"open /srv/missing.txt: no such file or directory",
	} {
		env := packageToolError("terminal", errors.New(raw))
		if !strings.Contains(env.ModelContent, raw) {
			t.Errorf("actionable error %q was redacted: %q", raw, env.ModelContent)
		}
		if env.ErrorCategory == "internal_state" {
			t.Errorf("actionable error %q misclassified as internal_state", raw)
		}
	}
}

// TestExplicitSafeMessageWinsOverLeakGuard pins precedence: a tool that
// describes its own failure keeps its wording and category.
func TestExplicitSafeMessageWinsOverLeakGuard(t *testing.T) {
	env := packageToolError("skill_select", stubStableFailure{
		code: "candidate_stale", category: "skill_state",
		safe: "the offered Skill candidates are stale", hint: "re-read the current candidates",
	})
	if env.ErrorCategory != "skill_state" || env.ErrorCode != "candidate_stale" {
		t.Fatalf("envelope=%+v", env)
	}
	if !strings.Contains(env.ModelContent, "stale") {
		t.Errorf("explicit safe message lost: %q", env.ModelContent)
	}
	if strings.Contains(env.ModelContent, "sql:") {
		t.Errorf("raw cause leaked: %q", env.ModelContent)
	}
}

// TestFailureWithOutputKeepsBothCauseAndOutputInCapture pins A-4: the capture
// was recomputed from stdout alone, so a typed error that also produced output
// left the real cause in neither the model surface nor the capture.
func TestFailureWithOutputKeepsBothCauseAndOutputInCapture(t *testing.T) {
	const stdout = "partial rows scanned before the failure"
	env := packageToolFailureCtx(nil, "session_search", stdout,
		errors.New("search failed: SQL logic error: no such column: 625 (1)"))

	if !strings.Contains(env.DiagnosticExcerpt, "no such column: 625") {
		t.Errorf("capture lost the raw cause: %q", env.DiagnosticExcerpt)
	}
	if !strings.Contains(env.DiagnosticExcerpt, stdout) {
		t.Errorf("capture lost the tool output: %q", env.DiagnosticExcerpt)
	}
	if env.DiagnosticBytes <= len(stdout) {
		t.Errorf("DiagnosticBytes=%d must describe the combined capture, not just stdout (%d)", env.DiagnosticBytes, len(stdout))
	}
	if strings.Contains(strings.ToLower(env.ModelContent), "sql logic error") {
		t.Errorf("raw cause reached the model surface: %q", env.ModelContent)
	}
	if !strings.Contains(env.ModelContent, stdout) {
		t.Errorf("model surface must still carry the tool output as evidence: %q", env.ModelContent)
	}
	if strings.Contains(strings.ToLower(env.Preview), "sql logic error") {
		t.Errorf("raw cause reached the user preview: %q", env.Preview)
	}
}

// TestSummaryPromptPlacesContractAboveOperatorGuidance pins A-11. The operator
// guidance banner claims the locked contract is "above" it, so the mandatory
// Relevant Files clause must actually precede the guidance — otherwise a line
// like "keep summaries under three lines" is positioned as the dominant
// instruction over a contract the model has not read yet, and a compaction can
// drop the file list the resumed agent depends on.
func TestSummaryPromptPlacesContractAboveOperatorGuidance(t *testing.T) {
	const operator = "Keep summaries under three lines."
	for name, prompt := range map[string]string{
		"initial": buildSummaryPromptWithGuidance("", "user: do the thing", operator),
		"update":  buildSummaryPromptWithGuidance("## Active Task\nprevious", "user: more", operator),
	} {
		contractAt := strings.Index(prompt, "## Relevant Files")
		guidanceAt := strings.Index(prompt, operator)
		bannerAt := strings.Index(prompt, "cannot override the locked output contract")
		if contractAt < 0 || guidanceAt < 0 || bannerAt < 0 {
			t.Fatalf("%s prompt missing a required part: contract=%d guidance=%d banner=%d\n%s",
				name, contractAt, guidanceAt, bannerAt, prompt)
		}
		if contractAt > guidanceAt {
			t.Errorf("%s prompt places operator guidance above the mandatory contract (contract=%d guidance=%d)",
				name, contractAt, guidanceAt)
		}
		if contractAt > bannerAt {
			t.Errorf("%s prompt: the banner claims the contract is above it, but the contract comes later", name)
		}
	}

	plain := buildSummaryPromptWithGuidance("", "user: do the thing")
	if strings.Contains(plain, "Operator-configured quality guidance") {
		t.Errorf("no operator guidance configured, yet the banner was emitted:\n%s", plain)
	}
	if !strings.Contains(plain, "## Relevant Files") {
		t.Errorf("the mandatory contract must always be present:\n%s", plain)
	}
	for _, required := range []string{"## Verification", "## Failed Attempts", "## Blockers and Waiting State", "<conversation-turns>", "untrusted state"} {
		if !strings.Contains(plain, required) {
			t.Errorf("summary handoff contract missing %q:\n%s", required, plain)
		}
	}
}
