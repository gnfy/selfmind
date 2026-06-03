package tools

import (
	"regexp"
	"strings"
)

var sensitivePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(authorization\s*[:=]\s*Bearer\s+)[A-Za-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`(?i)(api[_-]?key|token|secret|password|credential|authorization)\s*[:=]\s*["']?([^\s"',}]+)`),
	regexp.MustCompile(`(?i)(Bearer\s+)[A-Za-z0-9._~+/=-]{12,}`),
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),
}

func RedactSensitive(value string) string {
	if strings.TrimSpace(value) == "" {
		return value
	}
	out := value
	out = sensitivePatterns[0].ReplaceAllString(out, `${1}[REDACTED]`)
	out = sensitivePatterns[1].ReplaceAllString(out, `$1=[REDACTED]`)
	out = sensitivePatterns[2].ReplaceAllString(out, `${1}[REDACTED]`)
	out = sensitivePatterns[3].ReplaceAllString(out, `sk-[REDACTED]`)
	return out
}
