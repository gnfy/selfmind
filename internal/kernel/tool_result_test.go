package kernel

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPackageToolFailurePreservesBoundedDiagnosticEvidence(t *testing.T) {
	raw := strings.Repeat("stderr line with evidence\n", 200)
	env := packageToolFailureCtx(context.Background(), "terminal", raw, fmt.Errorf("exit status 1"))
	if env.DiagnosticHash == "" || env.DiagnosticBytes != len(raw) {
		t.Fatalf("diagnostic metadata = %#v", env)
	}
	if !env.DiagnosticTruncated || len(env.DiagnosticExcerpt) > 2100 {
		t.Fatalf("diagnostic excerpt is not bounded: %d bytes", len(env.DiagnosticExcerpt))
	}
	if !strings.Contains(env.ModelContent, "Captured tool output") || !strings.Contains(env.ModelContent, "stderr line") {
		t.Fatalf("model content lost failure evidence: %q", env.ModelContent)
	}
}

func TestPackageToolResultSplitsPreviewAndModelContent(t *testing.T) {
	raw := strings.Repeat("中文输出", 30000)

	env := packageToolResult("read_file", raw)

	if !env.Truncated {
		t.Fatalf("expected large result to be marked truncated")
	}
	if env.Preview == "" || len(env.Preview) > toolResultPreviewBytes+16 {
		t.Fatalf("unexpected preview length/content: len=%d value=%q", len(env.Preview), env.Preview)
	}
	if !strings.Contains(env.ModelContent, "tool output truncated for model context") {
		t.Fatalf("model content should explain bounded context: %q", env.ModelContent)
	}
	if !utf8.ValidString(env.Preview) || !utf8.ValidString(env.ModelContent) {
		t.Fatalf("packaged result must stay valid UTF-8")
	}
	if env.Raw != raw {
		t.Fatalf("raw result should be preserved separately")
	}
}

func TestPackageToolResultSummarizesListFilesPreview(t *testing.T) {
	raw := `{"path":".","entries":["a.go","b.go"],"count":2,"scanned":10,"truncated":true,"skipped_dirs":1}`

	env := packageToolResult("ls_r", raw)

	if env.Preview != "2 entries · 10 scanned · 1 dirs skipped · truncated" {
		t.Fatalf("preview = %q", env.Preview)
	}
	if env.ModelContent != raw {
		t.Fatalf("small raw JSON should still reach model unchanged")
	}
}

func TestPackageToolResultSummarizesPatchPreview(t *testing.T) {
	raw := `{"Success":true,"Diff":"","FilesModified":["/tmp/app.go"],"FilesCreated":["/tmp/new.go"],"FilesDeleted":["/tmp/old.go"]}`

	env := packageToolResult("patch", raw)

	if strings.Contains(env.Preview, `"Success"`) || strings.Contains(env.Preview, `"FilesModified"`) {
		t.Fatalf("preview should not expose raw JSON: %q", env.Preview)
	}
	for _, want := range []string{"modified /tmp/app.go", "created /tmp/new.go", "deleted /tmp/old.go"} {
		if !strings.Contains(env.Preview, want) {
			t.Fatalf("preview %q missing %q", env.Preview, want)
		}
	}
	if env.ModelContent != raw {
		t.Fatalf("small raw JSON should still reach model unchanged")
	}
}

func TestPackageToolErrorGuidesModelToDiagnose(t *testing.T) {
	env := packageToolError("terminal", errTest("exit status 1"))

	if !strings.Contains(env.Preview, "Error executing terminal") {
		t.Fatalf("preview should stay user-readable: %q", env.Preview)
	}
	if strings.Contains(env.Preview, "SelfMind diagnostic instruction") {
		t.Fatalf("preview should not expose model-only recovery instruction: %q", env.Preview)
	}
	if !strings.Contains(env.ModelContent, "SelfMind diagnostic instruction") {
		t.Fatalf("model content should include recovery instruction: %q", env.ModelContent)
	}
	if !strings.Contains(env.ModelContent, "inspect relevant context") {
		t.Fatalf("model content should nudge diagnosis before retry: %q", env.ModelContent)
	}
}

func TestPackageToolErrorDoesNotDuplicateEnrichedClassification(t *testing.T) {
	const hint = "The executable is not on PATH; verify the tool name or install it before retrying."
	err := fmt.Errorf("exec: executable file not found\nerror_class: exec_not_found; hint: %s", hint)
	env := packageToolError("terminal", err)

	if got := strings.Count(env.ModelContent, "error_class:"); got != 1 {
		t.Fatalf("error_class markers = %d, want 1: %q", got, env.ModelContent)
	}
	if got := strings.Count(env.ModelContent, "hint:"); got != 1 {
		t.Fatalf("hint markers = %d, want 1: %q", got, env.ModelContent)
	}
	if env.ErrorCategory != "exec_not_found" || env.RecoveryHint != hint {
		t.Fatalf("classification = %q / %q", env.ErrorCategory, env.RecoveryHint)
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }

type stableFailureTest struct{ cause string }

func (e stableFailureTest) Error() string             { return e.cause }
func (e stableFailureTest) ToolErrorCode() string     { return "session_search_unavailable" }
func (e stableFailureTest) ToolErrorCategory() string { return "data_store" }
func (e stableFailureTest) ModelSafeMessage() string  { return "Session history search is unavailable." }
func (e stableFailureTest) ToolRecoveryHint() string  { return "Retry once with simpler literal text." }

func TestPackageToolErrorSeparatesStableModelMessageFromLocalDiagnostic(t *testing.T) {
	env := packageToolError("session_search", stableFailureTest{cause: "SQL logic error: no such column: 625 (1)"})
	for _, want := range []string{
		"Session history search is unavailable.",
		"error_code: session_search_unavailable",
		"error_class: data_store",
		"Retry once with simpler literal text.",
	} {
		if !strings.Contains(env.ModelContent, want) {
			t.Fatalf("model content %q missing %q", env.ModelContent, want)
		}
	}
	if strings.Contains(env.ModelContent, "no such column") || strings.Contains(env.Preview, "no such column") {
		t.Fatalf("raw database diagnostic leaked to model or preview: %#v", env)
	}
	if !strings.Contains(env.DiagnosticExcerpt, "no such column: 625") || env.DiagnosticHash == "" {
		t.Fatalf("local diagnostic was not retained: %#v", env)
	}
	if env.ErrorCode != "session_search_unavailable" || env.ErrorCategory != "data_store" {
		t.Fatalf("stable failure metadata = %#v", env)
	}
}

// TestPackageToolErrorUserRejection ensures a user rejection is presented to
// the model as a decision with a do-not-retry instruction, not as a
// diagnosable failure — otherwise the model retries a variant of the rejected
// command and spawns a fresh approval (observed live via /reject on WeChat).
func TestPackageToolErrorUserRejection(t *testing.T) {
	rejected := packageToolError("terminal", fmt.Errorf("operation rejected: user said no"))
	if !strings.Contains(rejected.ModelContent, "Do NOT retry") {
		t.Fatalf("rejection should carry a do-not-retry instruction, got: %s", rejected.ModelContent)
	}
	if strings.Contains(rejected.ModelContent, "corrected next step") {
		t.Fatalf("rejection must not carry the diagnose-and-retry instruction: %s", rejected.ModelContent)
	}

	cancelled := packageToolError("terminal", fmt.Errorf("operation cancelled by user"))
	if !strings.Contains(cancelled.ModelContent, "Do NOT retry") {
		t.Fatalf("cancellation should carry a do-not-retry instruction, got: %s", cancelled.ModelContent)
	}

	ordinary := packageToolError("terminal", fmt.Errorf("exit status 1"))
	if !strings.Contains(ordinary.ModelContent, "corrected next step") {
		t.Fatalf("ordinary failures keep the diagnostic instruction, got: %s", ordinary.ModelContent)
	}
}

// TestIsUserRejectionErrDistinguishesHardFloor guards the contract that the
// hard-floor deny string from tools.SmartApprovalMiddleware ("operation blocked
// by safety policy: ...") is NOT treated as a user rejection. A hard block is a
// safety-policy decision, not a user preference; conflating the two would apply
// the wrong model instruction. The rejection strings must still match.
func TestIsUserRejectionErrDistinguishesHardFloor(t *testing.T) {
	blocked := fmt.Errorf("operation blocked by safety policy: recursive delete of protected root: / (do not retry; this is a hard safety limit, not a user rejection)")
	if isUserRejectionErr(blocked) {
		t.Fatalf("hard-floor block must NOT be classified as a user rejection: %v", blocked)
	}
	if !isUserRejectionErr(fmt.Errorf("operation rejected: user said no")) {
		t.Fatalf("user rejection must still be classified as a rejection")
	}
	if !isUserRejectionErr(fmt.Errorf("operation cancelled by user")) {
		t.Fatalf("user cancellation must still be classified as a rejection")
	}
}

// TestIsUserRejectionErrMatchesSafetyTriageDeny locks the H2 contract: a
// smart-mode triage DENY ("operation rejected: blocked by safety triage") is a
// decision, so the model must get the do-not-retry instruction, not
// diagnose-and-retry. It shares the user-rejection prefix on purpose.
func TestIsUserRejectionErrMatchesSafetyTriageDeny(t *testing.T) {
	deny := fmt.Errorf("operation rejected: blocked by safety triage")
	if !isUserRejectionErr(deny) {
		t.Fatalf("triage DENY must be classified as a do-not-retry decision: %v", deny)
	}
	env := packageToolError("terminal", deny)
	if !strings.Contains(env.ModelContent, "Do NOT retry") {
		t.Fatalf("triage DENY should carry a do-not-retry instruction, got: %s", env.ModelContent)
	}
}
