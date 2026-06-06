package tools

import (
	"fmt"
	"path/filepath"
	"strings"
)

func fuzzyReplace(content, oldText, newText string, replaceAll bool) (string, int, string, error) {
	if oldText == "" {
		return "", 0, "", fmt.Errorf("old_text is required")
	}

	if strings.Contains(content, oldText) {
		count := strings.Count(content, oldText)
		if !replaceAll && count != 1 {
			return "", 0, "", fmt.Errorf("old_text matched %d times; provide more context or set replace_all=true", count)
		}
		n := 1
		if replaceAll {
			n = -1
		}
		return strings.Replace(content, oldText, newText, n), countForReplace(count, replaceAll), "exact", nil
	}

	normalizedContent := normalizeNewlines(content)
	normalizedOld := normalizeNewlines(oldText)
	if normalizedContent != content || normalizedOld != oldText {
		if strings.Contains(normalizedContent, normalizedOld) {
			count := strings.Count(normalizedContent, normalizedOld)
			if !replaceAll && count != 1 {
				return "", 0, "", fmt.Errorf("old_text matched %d times after newline normalization; provide more context or set replace_all=true", count)
			}
			n := 1
			if replaceAll {
				n = -1
			}
			return strings.Replace(normalizedContent, normalizedOld, normalizeNewlines(newText), n), countForReplace(count, replaceAll), "newline-normalized", nil
		}
	}

	if updated, matches, ok := replaceTrimmedLineBlock(content, oldText, newText, replaceAll); ok {
		return updated, matches, "trimmed-line-block", nil
	}

	if updated, matches, ok := replaceCaseInsensitive(content, oldText, newText, replaceAll); ok {
		return updated, matches, "case-insensitive", nil
	}

	return "", 0, "", fmt.Errorf("old_text not found")
}

func countForReplace(count int, replaceAll bool) int {
	if replaceAll {
		return count
	}
	return 1
}

func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

func replaceCaseInsensitive(content, oldText, newText string, replaceAll bool) (string, int, bool) {
	lowerContent := strings.ToLower(content)
	lowerOld := strings.ToLower(oldText)
	var spans [][2]int
	start := 0
	for {
		idx := strings.Index(lowerContent[start:], lowerOld)
		if idx < 0 {
			break
		}
		from := start + idx
		to := from + len(oldText)
		spans = append(spans, [2]int{from, to})
		start = to
	}
	if len(spans) == 0 {
		return "", 0, false
	}
	if !replaceAll && len(spans) != 1 {
		return "", 0, false
	}
	if !replaceAll {
		span := spans[0]
		return content[:span[0]] + newText + content[span[1]:], 1, true
	}
	var sb strings.Builder
	last := 0
	for _, span := range spans {
		sb.WriteString(content[last:span[0]])
		sb.WriteString(newText)
		last = span[1]
	}
	sb.WriteString(content[last:])
	return sb.String(), len(spans), true
}

func replaceTrimmedLineBlock(content, oldText, newText string, replaceAll bool) (string, int, bool) {
	lines := strings.SplitAfter(content, "\n")
	oldLines := strings.Split(normalizeNewlines(strings.TrimSpace(oldText)), "\n")
	if len(oldLines) == 0 {
		return "", 0, false
	}
	for i := range oldLines {
		oldLines[i] = strings.TrimSpace(oldLines[i])
	}

	offsets := make([]int, len(lines)+1)
	pos := 0
	for i, line := range lines {
		offsets[i] = pos
		pos += len(line)
	}
	offsets[len(lines)] = pos

	var spans [][2]int
	for i := 0; i+len(oldLines) <= len(lines); i++ {
		matched := true
		for j := range oldLines {
			if strings.TrimSpace(strings.TrimSuffix(lines[i+j], "\n")) != oldLines[j] {
				matched = false
				break
			}
		}
		if matched {
			spans = append(spans, [2]int{offsets[i], offsets[i+len(oldLines)]})
		}
	}
	if len(spans) == 0 {
		return "", 0, false
	}
	if !replaceAll && len(spans) != 1 {
		return "", 0, false
	}
	if !replaceAll {
		span := spans[0]
		return content[:span[0]] + newText + content[span[1]:], 1, true
	}
	var sb strings.Builder
	last := 0
	for _, span := range spans {
		sb.WriteString(content[last:span[0]])
		sb.WriteString(newText)
		last = span[1]
	}
	sb.WriteString(content[last:])
	return sb.String(), len(spans), true
}

func patchFailureHint(content, oldText, target string) string {
	needle := firstMeaningfulNeedle(oldText)
	if needle == "" {
		return fmt.Sprintf("Patch failed in %s. Provide a non-empty old_text with surrounding context.", filepath.Base(target))
	}
	lowerContent := strings.ToLower(content)
	idx := strings.Index(lowerContent, strings.ToLower(needle))
	if idx < 0 {
		if len(content) > 800 {
			content = content[:800] + "\n...(truncated)"
		}
		return fmt.Sprintf("Patch failed in %s. No nearby match for %q.\n\nFile preview:\n%s", filepath.Base(target), needle, content)
	}
	start := idx - 300
	if start < 0 {
		start = 0
	}
	end := idx + 300
	if end > len(content) {
		end = len(content)
	}
	return fmt.Sprintf("Patch failed in %s. Nearby context for %q:\n%s", filepath.Base(target), needle, content[start:end])
}

func firstMeaningfulNeedle(oldText string) string {
	for _, line := range strings.Split(oldText, "\n") {
		line = strings.TrimSpace(line)
		if len(line) >= 8 {
			return line
		}
	}
	text := strings.TrimSpace(oldText)
	if len(text) > 40 {
		return text[:40]
	}
	return text
}
