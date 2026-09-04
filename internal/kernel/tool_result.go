package kernel

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/platform/textutil"
)

const (
	toolResultPreviewBytes = 800
	toolResultModelBytes   = 24000
	toolResultSeparator    = " · "
	// toolResultRawCapBytes bounds what is CAPTURED, codex-style: beyond this
	// the middle is dropped at intake and exists nowhere (not even in the
	// artifact spool). Protects daemon memory from a runaway command.
	toolResultRawCapBytes = 2 << 20 // 2 MiB
	// toolResultAgedBytes is the shrink target for artifact-backed tool
	// results that have aged out of the recent iterations of the SAME turn:
	// the model can read the full output back by reference at any time, so
	// aging them down is lossless (unlike codex, where the middle is gone).
	toolResultAgedBytes = 4096
	// toolArtifactNoteToken marks a model-surface truncation note that carries
	// an artifact reference. The aged-shrink pass in the agent loop only
	// shrinks messages containing it — shrinking is only safe when the full
	// output remains addressable.
	toolArtifactNoteToken = "saved as artifact "
)

// ToolResultEnvelope separates the same tool output into surfaces with
// different contracts: raw execution output, concise UI/event preview, and the
// bounded content sent back to the model.
type ToolResultEnvelope struct {
	Raw                 string
	Preview             string
	DisplayError        string
	ModelContent        string
	ErrorCode           string
	ErrorCategory       string
	RecoveryHint        string
	FailurePhase        string
	Retryability        string
	EffectState         string
	StateChanged        bool
	Alternatives        []string
	Truncated           bool
	Bytes               int
	DiagnosticExcerpt   string
	DiagnosticHash      string
	DiagnosticBytes     int
	DiagnosticTruncated bool
}

type stableToolFailure interface {
	error
	ToolErrorCode() string
	ToolErrorCategory() string
	ModelSafeMessage() string
	ToolRecoveryHint() string
}

type recoveryAwareToolFailure interface {
	ToolFailurePhase() string
	ToolRetryability() string
	ToolEffectState() string
	ToolStateChanged() bool
	ToolAlternatives() []string
}

func packageToolResult(name, raw string) ToolResultEnvelope {
	return packageToolResultCtx(context.Background(), name, raw)
}

// packageToolResultCtx builds the result envelope. When the model surface has
// to be truncated and the run carries a ToolArtifactSink, the capture-capped
// full output is spooled as an artifact and the truncation note tells the
// model to read omitted ranges via tool_output_view instead of re-running the
// command. Any sink failure degrades to the plain head/tail note — spooling
// must never fail a tool call.
func packageToolResultCtx(ctx context.Context, name, raw string) ToolResultEnvelope {
	raw = textutil.CleanUTF8(raw)
	if len(raw) > toolResultRawCapBytes {
		marker := fmt.Sprintf(
			"\n\n... [SelfMind note: output exceeded the %dMB capture limit; %d bytes from the middle were dropped at capture and are not recoverable. Narrow the command if the middle matters.] ...\n\n",
			toolResultRawCapBytes>>20, len(raw)-toolResultRawCapBytes,
		)
		raw = textutil.HeadTail(raw, (toolResultRawCapBytes-len(marker))/2, marker)
	}
	env := ToolResultEnvelope{
		Raw:     raw,
		Preview: toolResultPreview(name, raw),
		Bytes:   len(raw),
	}
	env.ModelContent, env.Truncated = toolResultModelContent(raw)
	if !env.Truncated {
		return env
	}
	sink := ToolArtifactSinkFromContext(ctx)
	if sink == nil {
		return env
	}
	ref, err := sink.SaveToolOutput(ctx, name, raw)
	if err != nil || strings.TrimSpace(ref.ID) == "" {
		return env
	}
	marker := fmt.Sprintf(
		"\n\n... [SelfMind note: tool output truncated for model context; the full %d-byte output was %s%s. Beginning and ending are shown. To read any omitted range, call tool_output_view with {\"artifact_id\": %q, \"offset_bytes\": N, \"limit_bytes\": M} instead of re-running the command.] ...\n\n",
		len(raw), toolArtifactNoteToken, ref.ID, ref.ID,
	)
	keep := (toolResultModelBytes - len(marker)) / 2
	if keep < 1024 {
		keep = 1024
	}
	env.ModelContent = textutil.HeadTail(raw, keep, marker)
	return env
}

func packageToolError(name string, err error) ToolResultEnvelope {
	return packageToolErrorWithMetadata(name, err, ToolExecutionMetadata{})
}

// packageToolErrorWithMetadata uses trusted registry metadata when deciding
// whether a raw storage signature belongs to SelfMind. External tools may
// legitimately return SQL errors that are actionable model evidence and must
// not be rewritten as a control-store failure.
func packageToolErrorWithMetadata(name string, err error, metadata ToolExecutionMetadata) ToolResultEnvelope {
	rawError := textutil.CleanUTF8(err.Error())
	userRejected := isUserRejectionErr(err)
	safeError := rawError
	code := ""
	category, hint := classifyToolFailure(rawError)
	var stable stableToolFailure
	if errors.As(err, &stable) {
		code = strings.TrimSpace(stable.ToolErrorCode())
		if value := strings.TrimSpace(stable.ToolErrorCategory()); value != "" {
			category = value
		}
		if value := strings.TrimSpace(stable.ModelSafeMessage()); value != "" {
			safeError = value
		}
		if value := strings.TrimSpace(stable.ToolRecoveryHint()); value != "" {
			hint = value
		}
	}
	failurePhase := ""
	retryability := ""
	effectState := ""
	stateChanged := false
	var alternatives []string
	var recovery recoveryAwareToolFailure
	if errors.As(err, &recovery) {
		failurePhase = strings.TrimSpace(recovery.ToolFailurePhase())
		retryability = strings.TrimSpace(recovery.ToolRetryability())
		effectState = strings.TrimSpace(recovery.ToolEffectState())
		stateChanged = recovery.ToolStateChanged()
		for _, alternative := range recovery.ToolAlternatives() {
			if value := strings.TrimSpace(alternative); value != "" {
				alternatives = append(alternatives, value)
			}
		}
	}
	// Boundary enforcement: the typed envelope is opt-in per call site, so a
	// tool that never wraps its error used to hand raw driver text straight to
	// the model (observed live: "resolve work unit: sql: no rows in result set"
	// and "SQL logic error: no such column: 625"). An explicit ModelSafeMessage
	// above always wins; this only catches the unwrapped internal-storage class,
	// whose Go/driver signatures are stable. The raw cause is still captured
	// below, so capture_ref remains diagnosable.
	if safeError == rawError && !strings.EqualFold(strings.TrimSpace(metadata.Origin), "external") {
		if replacement, leakCategory, leakHint, ok := internalStorageErrorLeak(rawError); ok {
			safeError = replacement
			category = leakCategory
			hint = leakHint
			if code == "" {
				code = "internal_state"
			}
		}
	}
	msg := fmt.Sprintf("Error executing %s: %s", nonEmpty(name, "tool"), safeError)
	if code != "" {
		msg += "\nerror_code: " + code
	}
	_, _, alreadyClassified := structuredToolFailureMarker(safeError)
	if category != "" && !userRejected && !alreadyClassified {
		msg += fmt.Sprintf("\nerror_class: %s; hint: %s", category, hint)
	}
	if failurePhase != "" {
		msg += "\nfailure_phase: " + failurePhase
	}
	if retryability != "" {
		msg += "\nretryability: " + retryability
	}
	if effectState != "" {
		msg += "\neffect_state: " + effectState
	}
	if len(alternatives) > 0 {
		msg += "\nalternatives: " + strings.Join(alternatives, ", ")
	}
	msg = textutil.CleanUTF8(msg)
	// A user rejection is a decision, not a failure. The generic
	// diagnose-and-retry guidance below is exactly what made the model retry a
	// variant of a rejected command (observed live: /reject spawned a fresh
	// approval for a tweaked command). Kernel must not import concrete tools,
	// so the stable "operation rejected"/"operation cancelled by user" error
	// strings from tools.SmartApprovalMiddleware are the documented contract.
	instruction := "\n\nSelfMind diagnostic instruction: this tool failed. Treat the error as evidence, inspect relevant context such as cwd, files, environment, auth state, provider constraints, or command help, and continue with a corrected next step unless this is a confirmed blocker."
	if userRejected {
		instruction = "\n\nSelfMind instruction: the USER explicitly rejected this operation. This is a decision, not an error. Do NOT retry this operation or any variant of it in this turn. Acknowledge the rejection, state briefly what was not done, and either propose a genuinely different approach for the user to confirm or finish the turn."
	}
	modelContent := msg + instruction
	envelope := ToolResultEnvelope{
		Raw:           msg,
		Preview:       textutil.Truncate(msg, toolResultPreviewBytes),
		DisplayError:  compactToolDisplayError(safeError, effectState),
		ModelContent:  textutil.Truncate(modelContent, 4000),
		ErrorCode:     code,
		ErrorCategory: category,
		RecoveryHint:  hint,
		FailurePhase:  failurePhase,
		Retryability:  retryability,
		EffectState:   effectState,
		StateChanged:  stateChanged,
		Alternatives:  append([]string(nil), alternatives...),
		Truncated:     len(modelContent) > 4000,
		Bytes:         len(msg),
	}
	if safeError != rawError {
		digest := sha256.Sum256([]byte(rawError))
		envelope.DiagnosticHash = fmt.Sprintf("%x", digest[:])
		envelope.DiagnosticBytes = len(rawError)
		envelope.DiagnosticExcerpt = textutil.Truncate(rawError, 2048)
		envelope.DiagnosticTruncated = len(rawError) > 2048
	}
	return envelope
}

// compactToolDisplayError is the human-facing error surface. Structured
// recovery metadata and diagnostic instructions belong in ModelContent and
// durable event fields; repeating them in the transcript makes a recoverable
// attempt look like a fatal application error.
func compactToolDisplayError(message, effectState string) string {
	message = textutil.CleanUTF8(message)
	var lines []string
	for _, line := range strings.Split(message, "\n") {
		line = strings.TrimSpace(line)
		lower := strings.ToLower(line)
		if line == "" || strings.HasPrefix(lower, "error_code:") ||
			strings.HasPrefix(lower, "error_class:") || strings.HasPrefix(lower, "failure_phase:") ||
			strings.HasPrefix(lower, "retryability:") || strings.HasPrefix(lower, "effect_state:") ||
			strings.HasPrefix(lower, "alternatives:") || strings.HasPrefix(lower, "selfmind diagnostic instruction:") ||
			strings.HasPrefix(lower, "selfmind instruction:") {
			continue
		}
		lines = append(lines, line)
		if len(lines) == 2 {
			break
		}
	}
	brief := strings.Join(lines, " ")
	if brief == "" {
		brief = "The tool could not complete this attempt."
	}
	brief = textutil.Truncate(brief, toolResultPreviewBytes)
	if strings.EqualFold(strings.TrimSpace(effectState), "not_dispatched") {
		return "Skipped before execution: " + brief
	}
	return brief
}

// internalStorageErrorLeak recognizes unwrapped internal storage failures whose
// text is an implementation detail, not evidence a model can act on. The set is
// deliberately narrow and anchored on stable database/sql and SQLite driver
// strings rather than open-ended prose matching: this is a leak guard, not a
// classifier. Tools that describe their own failure through stableToolFailure
// bypass it entirely.
func internalStorageErrorLeak(rawError string) (safe, category, hint string, ok bool) {
	lower := strings.ToLower(rawError)
	signatures := []string{
		"sql: no rows in result set",
		"sql: database is closed",
		"sql: transaction has already been committed",
		"sql logic error",
		"sqlite_",
		"no such column",
		"no such table",
		"constraint failed",
		"database is locked",
	}
	for _, signature := range signatures {
		if strings.Contains(lower, signature) {
			return "the requested record could not be read from local storage; the raw cause is recorded in local diagnostics",
				"internal_state",
				"This is a SelfMind storage-layer failure, not a problem with your arguments. Re-read the current state through a listing tool instead of retrying the same lookup.",
				true
		}
	}
	return "", "", "", false
}

// packageToolFailureCtx preserves bounded execution evidence when a tool
// returns both output and an error. Historically the output was discarded,
// leaving durable events with only "exit status 1" and making failures
// impossible to classify or learn from after the turn ended.
func packageToolFailureCtx(ctx context.Context, name, raw string, err error) ToolResultEnvelope {
	return packageDispatchedToolFailureCtx(ctx, name, raw, err, ToolExecutionMetadata{})
}

// packageDispatchedToolFailureCtx is the production dispatch boundary. The
// metadata argument is deliberately required so a caller cannot accidentally
// apply SelfMind's internal-storage redaction policy to an external tool.
func packageDispatchedToolFailureCtx(ctx context.Context, name, raw string, err error, metadata ToolExecutionMetadata) ToolResultEnvelope {
	_ = ctx
	if strings.TrimSpace(raw) == "" {
		return packageToolErrorWithMetadata(name, err, metadata)
	}
	raw = textutil.CleanUTF8(raw)
	errEnv := packageToolErrorWithMetadata(name, err, metadata)
	// The capture must carry BOTH the redacted-out raw cause and the tool
	// output. Recomputing it from stdout alone dropped errEnv's capture, so a
	// typed error that also produced output left the real cause in neither the
	// model surface nor the diagnostic surface, making capture_ref useless.
	diagnosticSource := raw
	if errEnv.DiagnosticExcerpt != "" {
		diagnosticSource = "Tool error cause:\n" + textutil.CleanUTF8(err.Error()) + "\n\nCaptured tool output:\n" + raw
	}
	digest := sha256.Sum256([]byte(diagnosticSource))
	const excerptBytes = 2048
	excerpt := diagnosticSource
	truncated := false
	if len(excerpt) > excerptBytes {
		marker := "\n... [diagnostic output truncated] ...\n"
		excerpt = textutil.HeadTail(excerpt, (excerptBytes-len(marker))/2, marker)
		truncated = true
	}
	// The model-facing body keeps only the safe error plus bounded tool output.
	outputExcerpt := raw
	if len(outputExcerpt) > excerptBytes {
		marker := "\n... [diagnostic output truncated] ...\n"
		outputExcerpt = textutil.HeadTail(outputExcerpt, (excerptBytes-len(marker))/2, marker)
		truncated = true
	}
	combined := fmt.Sprintf("%s\n\nCaptured tool output:\n%s", errEnv.Raw, outputExcerpt)
	modelContent := combined + "\n\nSelfMind diagnostic instruction: use the captured output as evidence. Correct the next step rather than repeating the same failing call."
	return ToolResultEnvelope{
		Raw: combined,
		// Preview is a user-facing surface, so it shows the tool's own output
		// rather than the diagnostic capture that carries the raw cause.
		Preview:             textutil.Truncate(outputExcerpt, toolResultPreviewBytes),
		DisplayError:        errEnv.DisplayError,
		ModelContent:        textutil.Truncate(modelContent, 6000),
		ErrorCode:           errEnv.ErrorCode,
		ErrorCategory:       errEnv.ErrorCategory,
		RecoveryHint:        errEnv.RecoveryHint,
		FailurePhase:        errEnv.FailurePhase,
		Retryability:        errEnv.Retryability,
		EffectState:         errEnv.EffectState,
		StateChanged:        errEnv.StateChanged,
		Alternatives:        append([]string(nil), errEnv.Alternatives...),
		Truncated:           truncated || len(modelContent) > 6000,
		Bytes:               len(raw),
		DiagnosticExcerpt:   excerpt,
		DiagnosticHash:      fmt.Sprintf("%x", digest[:]),
		DiagnosticBytes:     len(diagnosticSource),
		DiagnosticTruncated: truncated,
	}
}

// isUserRejectionErr detects an approval rejection/cancellation surfaced from
// the tools approval middleware. String matching is deliberate: kernel talks
// to tools only through the abstract backend, so these prefixes (kept stable
// in tools.SmartApprovalMiddleware) are the cross-package contract.
func isUserRejectionErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "operation rejected") ||
		strings.Contains(msg, "operation cancelled by user")
}

func nonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return fallback
}

func toolResultModelContent(raw string) (string, bool) {
	if len(raw) <= toolResultModelBytes {
		return raw, false
	}
	marker := fmt.Sprintf(
		"\n\n... [SelfMind note: tool output truncated for model context; original output was %d bytes. Beginning and ending are shown. Re-run a narrower tool call if omitted middle content matters.] ...\n\n",
		len(raw),
	)
	keep := (toolResultModelBytes - len(marker)) / 2
	if keep < 1024 {
		keep = 1024
	}
	return textutil.HeadTail(raw, keep, marker), true
}

// toolResultAgeIterations is how many agent-loop iterations an artifact-backed
// tool result stays verbatim before shrinkAgedToolResult ages it down.
const toolResultAgeIterations = 3

// toolResultTurnBudgetBytes bounds the TOTAL live tool-result bytes in one
// turn's working window.
//
// Per-result bounding and the age rule are both per-result, so several results
// just under toolResultModelBytes could legitimately coexist inside the age
// window: five of them is more context than the entire tool catalogue. Measured
// on real traffic, current_tool_results averaged about as much as tool_schemas
// and peaked far higher, which is a cumulative problem no per-result cap can
// see. This is the cumulative cap.
const toolResultTurnBudgetBytes = 32768

// liveToolResultBytes totals the tool-result bytes currently replayed to the
// model.
func liveToolResultBytes(messages []llm.Message) int {
	total := 0
	for _, msg := range messages {
		if msg.Role == "tool" {
			total += len(msg.Content)
		}
	}
	return total
}

// enforceToolResultTurnBudget ages artifact-backed tool results down,
// oldest-first, until the live total fits toolResultTurnBudgetBytes. It returns
// how many messages it shrank.
//
// Shrinking is lossless here: an artifact-backed result stays fully readable
// through tool_output_view, so the model loses proximity, not evidence. Results
// with no artifact reference are never touched — their bytes exist nowhere else,
// and dropping them to make room would trade a context saving for lost
// evidence. That means a turn full of small unspooled results can still exceed
// the budget, which is the correct failure direction.
func enforceToolResultTurnBudget(messages []llm.Message, indexes []int) int {
	total := liveToolResultBytes(messages)
	if total <= toolResultTurnBudgetBytes {
		return 0
	}
	shrunkCount := 0
	for _, index := range indexes {
		if total <= toolResultTurnBudgetBytes {
			break
		}
		if index < 0 || index >= len(messages) {
			continue
		}
		before := len(messages[index].Content)
		shrunk, ok := shrinkAgedToolResult(messages[index].Content)
		if !ok {
			continue
		}
		messages[index].Content = shrunk
		total -= before - len(shrunk)
		shrunkCount++
	}
	return shrunkCount
}

var toolArtifactIDPattern = regexp.MustCompile(`saved as artifact (art_[A-Za-z0-9_-]+)`)

// shrinkAgedToolResult ages one artifact-backed tool result out of the working
// window (codex-style history re-truncation, made lossless by the artifact
// spool): the head/tail shrink to toolResultAgedBytes around a note that keeps
// the artifact id readable, so the model can still fetch any byte range via
// tool_output_view. Content without an artifact reference is returned
// unchanged — shrinking is only safe when the full output stays addressable.
func shrinkAgedToolResult(content string) (string, bool) {
	if len(content) <= toolResultAgedBytes {
		return content, false
	}
	match := toolArtifactIDPattern.FindStringSubmatch(content)
	if match == nil {
		return content, false
	}
	note := fmt.Sprintf(
		"\n\n... [SelfMind note: this earlier tool output was aged out of the working window to save context; the full output is still readable via tool_output_view with {\"artifact_id\": %q, \"offset_bytes\": N, \"limit_bytes\": M}.] ...\n\n",
		match[1],
	)
	keep := (toolResultAgedBytes - len(note)) / 2
	if keep < 512 {
		keep = 512
	}
	return textutil.HeadTail(content, keep, note), true
}

func toolResultPreview(name, raw string) string {
	switch name {
	case "ls_r", "list_files":
		if summary := listFilesPreview(raw); summary != "" {
			return summary
		}
	case "search_files", "grep":
		if summary := searchFilesPreview(raw); summary != "" {
			return summary
		}
	case "update_plan":
		if summary := planPreview(raw); summary != "" {
			return summary
		}
	case "finish_run":
		if summary := finishRunPreview(raw); summary != "" {
			return summary
		}
	case "patch":
		if summary := patchPreview(raw); summary != "" {
			return summary
		}
	}
	if summary := genericJSONPreview(raw); summary != "" {
		return summary
	}
	return firstNonEmptyLine(raw, toolResultPreviewBytes)
}

func listFilesPreview(raw string) string {
	var payload struct {
		Count       int  `json:"count"`
		Scanned     int  `json:"scanned"`
		Truncated   bool `json:"truncated"`
		SkippedDirs int  `json:"skipped_dirs"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return ""
	}
	parts := []string{fmt.Sprintf("%d entries", payload.Count)}
	if payload.Scanned > payload.Count {
		parts = append(parts, fmt.Sprintf("%d scanned", payload.Scanned))
	}
	if payload.SkippedDirs > 0 {
		parts = append(parts, fmt.Sprintf("%d dirs skipped", payload.SkippedDirs))
	}
	if payload.Truncated {
		parts = append(parts, "truncated")
	}
	return strings.Join(parts, toolResultSeparator)
}

func searchFilesPreview(raw string) string {
	var payload struct {
		Count        int  `json:"count"`
		ScannedFiles int  `json:"scanned_files"`
		Truncated    bool `json:"truncated"`
		SkippedDirs  int  `json:"skipped_dirs"`
		SkippedLarge int  `json:"skipped_large"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return ""
	}
	parts := []string{fmt.Sprintf("%d matches", payload.Count)}
	if payload.ScannedFiles > 0 {
		parts = append(parts, fmt.Sprintf("%d files scanned", payload.ScannedFiles))
	}
	if payload.SkippedDirs > 0 {
		parts = append(parts, fmt.Sprintf("%d dirs skipped", payload.SkippedDirs))
	}
	if payload.SkippedLarge > 0 {
		parts = append(parts, fmt.Sprintf("%d large files skipped", payload.SkippedLarge))
	}
	if payload.Truncated {
		parts = append(parts, "truncated")
	}
	return strings.Join(parts, toolResultSeparator)
}

func planPreview(raw string) string {
	var payload struct {
		Plan []struct {
			Step   string `json:"step"`
			Status string `json:"status"`
		} `json:"plan"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil || len(payload.Plan) == 0 {
		return ""
	}
	inProgress := ""
	completed := 0
	for _, step := range payload.Plan {
		switch step.Status {
		case "in_progress":
			inProgress = strings.TrimSpace(step.Step)
		case "completed":
			completed++
		}
	}
	if inProgress != "" {
		return fmt.Sprintf("%d steps%snow: %s", len(payload.Plan), toolResultSeparator, inProgress)
	}
	return fmt.Sprintf("%d steps%s%d completed", len(payload.Plan), toolResultSeparator, completed)
}

func finishRunPreview(raw string) string {
	var payload struct {
		Status  string `json:"status"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return ""
	}
	status := strings.TrimSpace(payload.Status)
	summary := strings.TrimSpace(payload.Summary)
	switch {
	case status != "" && summary != "":
		return status + toolResultSeparator + summary
	case summary != "":
		return summary
	case status != "":
		return status
	default:
		return ""
	}
}

func patchPreview(raw string) string {
	var payload struct {
		Success       bool     `json:"Success"`
		FilesModified []string `json:"FilesModified"`
		FilesCreated  []string `json:"FilesCreated"`
		FilesDeleted  []string `json:"FilesDeleted"`
		Error         string   `json:"Error"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return ""
	}
	if strings.TrimSpace(payload.Error) != "" && !payload.Success {
		return firstNonEmptyLine(payload.Error, toolResultPreviewBytes)
	}
	parts := make([]string, 0, 3)
	if len(payload.FilesModified) > 0 {
		parts = append(parts, fmt.Sprintf("modified %s", summarizePaths(payload.FilesModified)))
	}
	if len(payload.FilesCreated) > 0 {
		parts = append(parts, fmt.Sprintf("created %s", summarizePaths(payload.FilesCreated)))
	}
	if len(payload.FilesDeleted) > 0 {
		parts = append(parts, fmt.Sprintf("deleted %s", summarizePaths(payload.FilesDeleted)))
	}
	if len(parts) == 0 {
		if payload.Success {
			return "patch applied"
		}
		return ""
	}
	return strings.Join(parts, toolResultSeparator)
}

func summarizePaths(paths []string) string {
	cleaned := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path != "" {
			cleaned = append(cleaned, path)
		}
	}
	switch len(cleaned) {
	case 0:
		return "0 files"
	case 1:
		return cleaned[0]
	case 2:
		return cleaned[0] + ", " + cleaned[1]
	default:
		return fmt.Sprintf("%s, %s +%d more", cleaned[0], cleaned[1], len(cleaned)-2)
	}
}

func genericJSONPreview(raw string) string {
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &obj); err != nil || len(obj) == 0 {
		return ""
	}
	if msg := firstJSONString(obj, "message", "summary", "status", "error", "Error"); msg != "" {
		return textutil.Truncate(msg, toolResultPreviewBytes)
	}
	for _, key := range []string{"FilesModified", "files_modified", "modified", "files"} {
		if paths := jsonStringSlice(obj[key]); len(paths) > 0 {
			return "modified " + summarizePaths(paths)
		}
	}
	for _, key := range []string{"FilesCreated", "files_created", "created"} {
		if paths := jsonStringSlice(obj[key]); len(paths) > 0 {
			return "created " + summarizePaths(paths)
		}
	}
	for _, key := range []string{"FilesDeleted", "files_deleted", "deleted"} {
		if paths := jsonStringSlice(obj[key]); len(paths) > 0 {
			return "deleted " + summarizePaths(paths)
		}
	}
	if success, ok := jsonBool(obj, "Success", "success", "ok"); ok && success {
		return "completed"
	}
	return fmt.Sprintf("%d fields", len(obj))
}

func firstJSONString(obj map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if value, ok := obj[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func jsonStringSlice(value interface{}) []string {
	switch v := value.(type) {
	case []string:
		return v
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				out = append(out, strings.TrimSpace(text))
			}
		}
		return out
	default:
		return nil
	}
}

func jsonBool(obj map[string]interface{}, keys ...string) (bool, bool) {
	for _, key := range keys {
		if value, ok := obj[key].(bool); ok {
			return value, true
		}
	}
	return false, false
}

func firstNonEmptyLine(raw string, maxBytes int) string {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return textutil.Truncate(line, maxBytes)
		}
	}
	return ""
}
