package tools

import "strings"

// unifiedLineDiff produces a readable line diff between oldText and newText by
// trimming the common prefix/suffix and showing the changed middle as a removed
// block followed by an added block, with a few lines of surrounding context.
// It is not a minimal (Myers) diff — it is a correct, dependency-free diff good
// for display, used to render write_file overwrites instead of an opaque
// "all-added" dump. Returns the diff lines (" ctx" / "-old" / "+new") plus
// added/removed counts.
func unifiedLineDiff(oldText, newText string, contextLines int) (lines []string, added, removed int) {
	if contextLines < 0 {
		contextLines = 0
	}
	o := splitDiffLines(oldText)
	n := splitDiffLines(newText)

	// Common prefix.
	p := 0
	for p < len(o) && p < len(n) && o[p] == n[p] {
		p++
	}
	// Common suffix (not overlapping the prefix).
	s := 0
	for s < len(o)-p && s < len(n)-p && o[len(o)-1-s] == n[len(n)-1-s] {
		s++
	}

	// Leading context.
	start := p - contextLines
	if start < 0 {
		start = 0
	}
	for i := start; i < p; i++ {
		lines = append(lines, " "+o[i])
	}
	// Removed (old middle), then added (new middle).
	for i := p; i < len(o)-s; i++ {
		lines = append(lines, "-"+o[i])
		removed++
	}
	for i := p; i < len(n)-s; i++ {
		lines = append(lines, "+"+n[i])
		added++
	}
	// Trailing context.
	for i, c := len(o)-s, 0; i < len(o) && c < contextLines; i, c = i+1, c+1 {
		lines = append(lines, " "+o[i])
	}
	return lines, added, removed
}

func splitDiffLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
}
