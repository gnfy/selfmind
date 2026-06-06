package httpapi

import (
	"path/filepath"
	"strings"

	"selfmind/internal/gateway/api"
)

func buildRunOutcome(content string) api.RunOutcome {
	trimmed := strings.TrimSpace(content)
	out := api.RunOutcome{
		Status:  inferTaskStatus(trimmed),
		Summary: truncate(toOneLine(trimmed), 1000),
	}
	out.NeedApprove = looksTaskBlocked(trimmed) && containsAny(strings.ToLower(trimmed), []string{
		"approval",
		"approve",
		"permission",
		"confirm",
		"requires",
	})
	out.Done = extractOutcomeSection(trimmed, []string{"done", "completed", "changes", "finished", "已完成", "完成"})
	out.NextSteps = extractOutcomeSection(trimmed, []string{"next steps", "remaining", "todo", "下一步", "后续", "待办"})
	out.Risks = extractOutcomeSection(trimmed, []string{"risks", "warnings", "blocked", "风险", "阻塞"})
	out.Tests = extractTestLines(trimmed)
	out.Files = extractFileMentions(trimmed)
	return out
}

func extractOutcomeSection(content string, headings []string) []string {
	lines := strings.Split(content, "\n")
	var items []string
	capturing := false
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			if capturing && len(items) > 0 {
				break
			}
			continue
		}
		normalized := strings.Trim(strings.ToLower(line), "#*: ")
		if matchesHeading(normalized, headings) {
			capturing = true
			continue
		}
		if capturing && looksLikeHeading(normalized) {
			break
		}
		if capturing {
			if item := cleanOutcomeItem(line); item != "" {
				items = appendUnique(items, item, 8)
			}
		}
	}
	return items
}

func matchesHeading(line string, headings []string) bool {
	for _, h := range headings {
		h = strings.ToLower(h)
		if line == h || strings.HasPrefix(line, h+":") || strings.HasPrefix(line, h+"：") {
			return true
		}
	}
	return false
}

func looksLikeHeading(line string) bool {
	if strings.HasSuffix(line, ":") || strings.HasSuffix(line, "：") {
		return true
	}
	for _, heading := range []string{"summary", "done", "completed", "next steps", "risks", "tests", "files", "摘要", "完成", "下一步", "风险", "测试", "文件"} {
		if line == heading {
			return true
		}
	}
	return false
}

func cleanOutcomeItem(line string) string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "-")
	line = strings.TrimPrefix(line, "*")
	line = strings.TrimPrefix(line, "•")
	line = strings.TrimSpace(line)
	if len(line) > 2 && line[1] == '.' && line[0] >= '0' && line[0] <= '9' {
		line = strings.TrimSpace(line[2:])
	}
	return truncate(line, 220)
}

func extractTestLines(content string) []string {
	var out []string
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if looksLikeHeading(strings.Trim(strings.ToLower(line), "#*: ")) {
			continue
		}
		lower := strings.ToLower(line)
		if strings.Contains(lower, "test") || strings.Contains(lower, "go test") || strings.Contains(lower, "npm test") || strings.Contains(line, "测试") {
			out = appendUnique(out, truncate(line, 220), 6)
		}
	}
	return out
}

func extractFileMentions(content string) []string {
	var out []string
	separators := func(r rune) bool {
		return r == ' ' || r == '\n' || r == '\t' || r == ',' || r == ';' || r == '，' || r == '；' || r == ')' || r == '('
	}
	for _, token := range strings.FieldsFunc(content, separators) {
		token = strings.Trim(token, "`'\"[]{}<>")
		if looksLikePath(token) {
			out = appendUnique(out, filepath.ToSlash(token), 12)
		}
	}
	return out
}

func looksLikePath(token string) bool {
	if len(token) < 4 || len(token) > 180 {
		return false
	}
	if strings.Contains(token, "://") {
		return false
	}
	ext := strings.ToLower(filepath.Ext(token))
	if ext == "" {
		return false
	}
	switch ext {
	case ".go", ".md", ".yaml", ".yml", ".json", ".toml", ".txt", ".ts", ".tsx", ".js", ".jsx", ".py", ".sh", ".ps1", ".sql", ".html", ".css":
		return strings.Contains(token, "/") || strings.Contains(token, "\\") || strings.HasPrefix(token, "cmd") || strings.HasPrefix(token, "internal") || strings.HasPrefix(token, "docs")
	default:
		return false
	}
}

func appendUnique(items []string, item string, max int) []string {
	item = strings.TrimSpace(item)
	if item == "" {
		return items
	}
	for _, existing := range items {
		if existing == item {
			return items
		}
	}
	if max > 0 && len(items) >= max {
		return items
	}
	return append(items, item)
}
