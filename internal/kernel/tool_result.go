package kernel

import (
	"encoding/json"
	"fmt"
	"strings"

	"selfmind/internal/platform/textutil"
)

const (
	toolResultPreviewBytes = 800
	toolResultModelBytes   = 64000
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
	return ToolResultEnvelope{
		Raw:          msg,
		Preview:      textutil.Truncate(msg, toolResultPreviewBytes),
		ModelContent: textutil.Truncate(msg, 4000),
		Truncated:    len(msg) > 4000,
		Bytes:        len(msg),
	}
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
	return strings.Join(parts, " · ")
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
	return strings.Join(parts, " · ")
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
		return fmt.Sprintf("%d steps · now: %s", len(payload.Plan), inProgress)
	}
	return fmt.Sprintf("%d steps · %d completed", len(payload.Plan), completed)
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
		return status + " · " + summary
	case summary != "":
		return summary
	case status != "":
		return status
	default:
		return ""
	}
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
