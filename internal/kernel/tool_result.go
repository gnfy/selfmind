package kernel

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

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
	ModelContent        string
	Truncated           bool
	Bytes               int
	DiagnosticExcerpt   string
	DiagnosticHash      string
	DiagnosticBytes     int
	DiagnosticTruncated bool
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
	msg := fmt.Sprintf("Error executing %s: %v", nonEmpty(name, "tool"), err)
	msg = textutil.CleanUTF8(msg)
	// A user rejection is a decision, not a failure. The generic
	// diagnose-and-retry guidance below is exactly what made the model retry a
	// variant of a rejected command (observed live: /reject spawned a fresh
	// approval for a tweaked command). Kernel must not import concrete tools,
	// so the stable "operation rejected"/"operation cancelled by user" error
	// strings from tools.SmartApprovalMiddleware are the documented contract.
	instruction := "\n\nSelfMind diagnostic instruction: this tool failed. Treat the error as evidence, inspect relevant context such as cwd, files, environment, auth state, provider constraints, or command help, and continue with a corrected next step unless this is a confirmed blocker."
	if isUserRejectionErr(err) {
		instruction = "\n\nSelfMind instruction: the USER explicitly rejected this operation. This is a decision, not an error. Do NOT retry this operation or any variant of it in this turn. Acknowledge the rejection, state briefly what was not done, and either propose a genuinely different approach for the user to confirm or finish the turn."
	}
	modelContent := msg + instruction
	return ToolResultEnvelope{
		Raw:          msg,
		Preview:      textutil.Truncate(msg, toolResultPreviewBytes),
		ModelContent: textutil.Truncate(modelContent, 4000),
		Truncated:    len(modelContent) > 4000,
		Bytes:        len(msg),
	}
}

// packageToolFailureCtx preserves bounded execution evidence when a tool
// returns both output and an error. Historically the output was discarded,
// leaving durable events with only "exit status 1" and making failures
// impossible to classify or learn from after the turn ended.
func packageToolFailureCtx(ctx context.Context, name, raw string, err error) ToolResultEnvelope {
	_ = ctx
	if strings.TrimSpace(raw) == "" {
		return packageToolError(name, err)
	}
	raw = textutil.CleanUTF8(raw)
	digest := sha256.Sum256([]byte(raw))
	const excerptBytes = 2048
	excerpt := raw
	truncated := false
	if len(excerpt) > excerptBytes {
		marker := "\n... [diagnostic output truncated] ...\n"
		excerpt = textutil.HeadTail(excerpt, (excerptBytes-len(marker))/2, marker)
		truncated = true
	}
	errEnv := packageToolError(name, err)
	combined := fmt.Sprintf("%s\n\nCaptured tool output:\n%s", errEnv.Raw, excerpt)
	modelContent := combined + "\n\nSelfMind diagnostic instruction: use the captured output as evidence. Correct the next step rather than repeating the same failing call."
	return ToolResultEnvelope{
		Raw:                 combined,
		Preview:             textutil.Truncate(excerpt, toolResultPreviewBytes),
		ModelContent:        textutil.Truncate(modelContent, 6000),
		Truncated:           truncated || len(modelContent) > 6000,
		Bytes:               len(raw),
		DiagnosticExcerpt:   excerpt,
		DiagnosticHash:      fmt.Sprintf("%x", digest[:]),
		DiagnosticBytes:     len(raw),
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
