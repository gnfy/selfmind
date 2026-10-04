package textutil

import (
	"strings"
	"unicode/utf8"
)

func CleanUTF8(s string) string {
	s = strings.ToValidUTF8(s, "")
	return strings.ReplaceAll(s, "\uFFFD", "")
}

func TruncateBytes(s string, max int) string {
	s = CleanUTF8(s)
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.ValidString(s[:cut]) {
		cut--
	}
	return s[:cut]
}

// RuneBoundary returns the start of the character that contains byte i of s,
// so s[:RuneBoundary(s, i)] never ends inside a multi-byte character. It
// returns len(s) when i is past the end.
func RuneBoundary(s string, i int) int {
	if i <= 0 {
		return 0
	}
	if i >= len(s) {
		return len(s)
	}
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

func Truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	s = CleanUTF8(s)
	if len(s) <= max {
		return s
	}
	return TruncateBytes(s, max) + "..."
}

func HeadTail(s string, keepBytes int, marker string) string {
	s = CleanUTF8(s)
	if keepBytes <= 0 || len(s) <= keepBytes*2 {
		return s
	}
	head := TruncateBytes(s, keepBytes)
	return head + marker + TailBytes(s, keepBytes)
}

func TailBytes(s string, max int) string {
	s = CleanUTF8(s)
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	start := len(s) - max
	for start < len(s) && !utf8.ValidString(s[start:]) {
		start++
	}
	return s[start:]
}
