package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"unicode/utf8"
)

type RunSnapshot struct {
	Output            string
	ToolCalls         int
	ActionToolCalls   int
	ToolErrors        int
	Errors            []string
	ErrorCategories   map[string]int
	TaskIDs           []string
	Workspace         string
	ExpectedWorkspace string
	DurationSeconds   float64
	OutcomeStatus     string
}

type CheckResult struct {
	Name    string  `json:"name"`
	OK      bool    `json:"ok"`
	Message string  `json:"message,omitempty"`
	Score   float64 `json:"score,omitempty"`
}

func EvaluateCase(c *Case, snap RunSnapshot) []CheckResult {
	var checks []CheckResult
	add := func(name string, ok bool, message string) {
		score := 0.0
		if ok {
			score = 1.0
		}
		checks = append(checks, CheckResult{Name: name, OK: ok, Message: message, Score: score})
	}

	output := snap.Output
	if c.Checks.NoEmptyResponse {
		add("no_empty_response", strings.TrimSpace(output) != "", "assistant output should not be empty")
	}
	if c.Checks.NoMojibake {
		add("no_mojibake", !hasMojibake(output), "output should not contain replacement-character mojibake")
	}
	if c.Checks.NoRawJSONLeak {
		add("no_raw_json_leak", !hasRawJSONLeak(output), "output should not expose raw tool or protocol JSON")
	}
	if c.Checks.NoToolXMLLeak {
		add("no_tool_xml_leak", !hasToolXMLLeak(output), "output should not expose raw <tool> protocol text")
	}
	if c.Checks.NoProviderStackDump {
		add("no_provider_stack_dump", !hasProviderStackDump(output), "provider errors should be summarized instead of dumped")
	}
	if c.Checks.ContextNotExceeded {
		add("context_not_exceeded", !hasContextOverflow(output, snap.Errors), "run should not overflow provider context")
	}
	if c.Expect.RequireToolEvents {
		add("require_tool_events", snap.ActionToolCalls > 0, "task expected visible action tool events")
	}
	if c.Expect.MinToolCalls > 0 {
		add("min_tool_calls", snap.ActionToolCalls >= c.Expect.MinToolCalls, "action tool call count below expectation")
	}
	if c.Expect.MaxToolCalls != nil {
		add("max_tool_calls", snap.ActionToolCalls <= *c.Expect.MaxToolCalls, "action tool call count above expectation")
	}
	if c.Expect.MaxToolErrors >= 0 && c.Expect.MaxToolErrors != 0 {
		add("max_tool_errors", snap.ToolErrors <= c.Expect.MaxToolErrors, "too many tool errors")
	}
	if c.Expect.MaxDurationSeconds > 0 {
		add("max_duration_seconds", snap.DurationSeconds <= float64(c.Expect.MaxDurationSeconds), "case exceeded duration budget")
	}
	if c.Expect.RequireSameTask && len(uniqueStrings(snap.TaskIDs)) > 1 {
		add("require_same_task", false, "multi-turn case switched task IDs")
	} else if c.Expect.RequireSameTask {
		add("require_same_task", true, "")
	}
	if c.Expect.RequireContinuation {
		add("require_continuation", len(c.Turns) > 1 && len(uniqueStrings(snap.TaskIDs)) == 1, "continuation should reuse the active task context")
	}
	if strings.TrimSpace(c.Expect.Status) != "" {
		want := normalizeStatus(c.Expect.Status)
		got := normalizeStatus(snap.OutcomeStatus)
		ok := got == want
		if want == "completed" && got == "" && strings.TrimSpace(output) != "" {
			ok = true
		}
		add("status:"+want, ok, "outcome status should match expectation; got "+firstNonEmpty(got, "<empty>"))
	}
	if c.Expect.RequireWorkspaceMatch || c.Checks.WorkspaceShouldMatch {
		want := strings.TrimSpace(snap.ExpectedWorkspace)
		got := strings.TrimSpace(snap.Workspace)
		add("workspace_should_match", want == "" || got == "" || samePathish(want, got), "workspace should match the case setting")
	}
	for _, needle := range c.Expect.Contains {
		needle = strings.TrimSpace(needle)
		if needle != "" {
			add("contains:"+needle, strings.Contains(output, needle), "output should contain expected text")
		}
	}
	for _, needle := range c.Expect.MustNotContain {
		needle = strings.TrimSpace(needle)
		if needle != "" {
			add("must_not_contain:"+needle, !strings.Contains(output, needle), "output should not contain disallowed text")
		}
	}
	if c.Checks.ToolFailureShouldRecover && snap.ToolErrors > 0 {
		add("tool_failure_should_recover", strings.TrimSpace(output) != "" && !strings.EqualFold(snap.OutcomeStatus, "blocked"), "tool failures should not automatically block the run")
	}
	return checks
}

func normalizeStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "done", "ok", "passed", "pass", "success", "succeeded", "completed", "complete":
		return "completed"
	case "fail", "failed", "error", "errored":
		return "failed"
	default:
		return strings.ToLower(strings.TrimSpace(status))
	}
}

func ChecksPassed(checks []CheckResult) bool {
	for _, c := range checks {
		if !c.OK {
			return false
		}
	}
	return true
}

func hasMojibake(s string) bool {
	if strings.Contains(s, "\ufffd") || strings.Contains(s, "���") || strings.Contains(s, "锟斤拷") {
		return true
	}
	for _, marker := range commonMojibakeMarkers {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return !utf8.ValidString(s)
}

var commonMojibakeMarkers = []string{
	"鍒嗘瀽",
	"涓€",
	"銆",
	"绠€",
	"鍙互",
	"鎸夋柟",
	"浠ｇ爜",
	"鐨勪",
	"瀹炵幇",
	"缁欏嚭",
	"杈撳嚭",
}

var rawJSONLeakRE = regexp.MustCompile(`(?s)(\{"tool_calls"\s*:|\{"plan"\s*:|\{"error"\s*:|\{"input"\s*:|\{"output"\s*:)`)

func hasRawJSONLeak(s string) bool {
	return rawJSONLeakRE.MatchString(s)
}

func hasToolXMLLeak(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, "<tool>") || strings.Contains(lower, "</tool>") ||
		strings.Contains(lower, "<parameter>") || strings.Contains(lower, "</parameter>")
}

func hasProviderStackDump(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, "llm stream chat failed after 3 attempts") ||
		strings.Contains(lower, "non-stream fallback failed") ||
		strings.Contains(lower, "responses api error") && strings.Contains(lower, "{")
}

func hasContextOverflow(output string, errors []string) bool {
	errText := strings.ToLower(strings.Join(errors, "\n"))
	if containsContextOverflowPhrase(errText) {
		return true
	}
	out := strings.ToLower(output)
	if !strings.Contains(out, "error") && !strings.Contains(out, "failed") && !strings.Contains(out, "exceed") {
		return false
	}
	return containsContextOverflowPhrase(out)
}

func containsContextOverflowPhrase(s string) bool {
	return strings.Contains(s, "context length") ||
		strings.Contains(s, "maximum context") ||
		strings.Contains(s, "too many tokens") ||
		strings.Contains(s, "context window") ||
		strings.Contains(s, "input is too long")
}

func classifyError(message string) string {
	lower := strings.ToLower(strings.TrimSpace(message))
	switch {
	case lower == "":
		return ""
	case strings.Contains(lower, "401") || strings.Contains(lower, "unauthorized") || strings.Contains(lower, "token_expired") || strings.Contains(lower, "authentication"):
		return "provider_auth"
	case strings.Contains(lower, "invalid schema") || strings.Contains(lower, "missing required parameter") || strings.Contains(lower, "tool call") || strings.Contains(lower, "schema"):
		return "tool_schema"
	case containsContextOverflowPhrase(lower):
		return "context_overflow"
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline exceeded"):
		return "timeout"
	case strings.Contains(lower, "unexpected eof") || strings.Contains(lower, "connection reset") || strings.Contains(lower, "stream"):
		return "provider_transport"
	case strings.Contains(lower, "escapes workspace") || strings.Contains(lower, "permission") || strings.Contains(lower, "denied"):
		return "workspace_scope"
	case strings.Contains(lower, "exit status") || strings.Contains(lower, "command failed"):
		return "command_failed"
	default:
		return "unknown"
	}
}

func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

func preview(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if maxRunes <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "...(truncated)"
}

func uniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func samePathish(a, b string) bool {
	a = strings.TrimRight(strings.ReplaceAll(strings.ToLower(strings.TrimSpace(a)), "\\", "/"), "/")
	b = strings.TrimRight(strings.ReplaceAll(strings.ToLower(strings.TrimSpace(b)), "\\", "/"), "/")
	return a == b
}
