package tools

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// approvalSummaryMaxLines bounds how much of a payload the summary scanner will
// walk. A patch big enough to exceed it is already "large" for display purposes,
// and an unbounded scan on the approval path would let one oversized tool call
// stall the ask.
const approvalSummaryMaxLines = 20000

// ApprovalChangeSummary renders a compact, CONTENT-FREE description of what a
// write-shaped tool call would change: file counts and line/byte counts only.
// It exists so an approval surface can show the SIZE of a change ("2 files
// +48/-12") without ever becoming a content channel — approval payloads are
// redacted for exactly that reason (approvalDisplayArgs), so this must not
// reintroduce content by another name. Returns "" when the call is not a write
// or carries nothing measurable.
func ApprovalChangeSummary(toolName string, args map[string]interface{}) string {
	if len(args) == 0 {
		return ""
	}
	if patch, ok := args["patch"].(string); ok && strings.TrimSpace(patch) != "" {
		return patchChangeSummary(patch)
	}
	if content, ok := args["content"].(string); ok && isWriteTool(toolName) {
		return contentChangeSummary(content)
	}
	if code, ok := args["code"].(string); ok && toolName == "execute_code" {
		language, _ := args["language"].(string)
		if strings.TrimSpace(language) == "" {
			language = "python"
		}
		return language + " script, " + contentChangeSummary(code)
	}
	return ""
}

// ApprovalPersistentArgs builds the durable, display-only argument envelope
// for an approval. Arbitrary code is execution input, not audit metadata: keep
// a bounded redacted preview and a digest, never the complete source.
func ApprovalPersistentArgs(toolName string, args map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{})
	for key, value := range args {
		if strings.HasPrefix(key, "_") || (toolName == "execute_code" && key == "code") {
			continue
		}
		out[key] = RedactSensitive(fmt.Sprintf("%v", value))
	}
	if toolName != "execute_code" {
		return out
	}
	code, _ := args["code"].(string)
	sum := sha256.Sum256([]byte(code))
	preview := code
	if lines := strings.Split(preview, "\n"); len(lines) > 20 {
		preview = strings.Join(lines[:20], "\n")
	}
	if len(preview) > 2048 {
		preview = preview[:2048]
	}
	out["code_lines"] = strings.Count(code, "\n") + 1
	out["code_bytes"] = len(code)
	out["code_sha256"] = fmt.Sprintf("%x", sum[:])
	out["code_preview"] = RedactSensitive(preview)
	return out
}

// patchChangeSummary counts V4A patch envelopes and their added/removed lines.
// The markers are the ones documented on the patch tool ("*** Add File:" and
// friends, internal/tools/patch.go); a hunk line's FIRST character is the
// add/remove signal, and "+++"/"---" headers are excluded so a unified-diff
// paste cannot inflate the counts.
func patchChangeSummary(patch string) string {
	files, added, removed := 0, 0, 0
	for i, line := range strings.Split(patch, "\n") {
		if i >= approvalSummaryMaxLines {
			break
		}
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "*** Add File:"),
			strings.HasPrefix(trimmed, "*** Update File:"),
			strings.HasPrefix(trimmed, "*** Delete File:"),
			strings.HasPrefix(trimmed, "*** Move File:"):
			files++
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
			// Unified-diff file headers, not content lines.
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			removed++
		}
	}
	var parts []string
	if files > 0 {
		parts = append(parts, pluralCount(files, "file"))
	}
	if added > 0 || removed > 0 {
		parts = append(parts, fmt.Sprintf("+%d/-%d", added, removed))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ")
}

// contentChangeSummary sizes a whole-file write. Line count plus a human byte
// size answers "is this a tweak or a rewrite" — the only question the panel
// needs the body for.
func contentChangeSummary(content string) string {
	if content == "" {
		return "empty file"
	}
	lines := strings.Count(content, "\n")
	if !strings.HasSuffix(content, "\n") {
		lines++
	}
	return fmt.Sprintf("%s, %s", pluralCount(lines, "line"), humanBytes(len(content)))
}

func pluralCount(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// humanBytes renders a byte count at one decimal place from KB up. Sizes on the
// approval path are small enough that MB is the practical ceiling.
func humanBytes(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}
