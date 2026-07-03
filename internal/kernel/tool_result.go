package kernel

import (
	"encoding/json"
	"fmt"
	"strings"

	"selfmind/internal/platform/textutil"
)

const (
	toolResultPreviewBytes = 800
	toolResultModelBytes   = 24000
	toolResultSeparator    = " · "
)

// ToolResultEnvelope separates the same tool output into surfaces with
// different contracts: raw execution output, concise UI/event preview, and the
// bounded content sent back to the model.
type ToolResultEnvelope struct {
	Raw          string
	Preview      string
	ModelContent string
	Truncated    bool
	Bytes        int
}

func packageToolResult(name, raw string) ToolResultEnvelope {
	raw = textutil.CleanUTF8(raw)
	env := ToolResultEnvelope{
		Raw:     raw,
		Preview: toolResultPreview(name, raw),
		Bytes:   len(raw),
	}
	env.ModelContent, env.Truncated = toolResultModelContent(raw)
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
