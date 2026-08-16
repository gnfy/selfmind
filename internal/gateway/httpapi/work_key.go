package httpapi

import (
	"regexp"
	"strings"
)

// taskWorkKeyPattern extracts deterministic issue/ticket identifiers. Work
// keys are display-governance hints only: they never select workspace,
// context, tools, permissions, or an execution policy.
var taskWorkKeyPattern = regexp.MustCompile(`(?i)\b([a-z][a-z0-9]{1,9}-\d{1,6})\b`)

func taskWorkKeys(values ...string) map[string]struct{} {
	keys := make(map[string]struct{})
	for _, value := range values {
		for _, match := range taskWorkKeyPattern.FindAllString(value, -1) {
			keys[strings.ToUpper(match)] = struct{}{}
		}
	}
	return keys
}

// uniqueTaskWorkKey returns a key only when the evidence names exactly one.
// Ambiguous multi-ticket turns remain under model/human governance.
func uniqueTaskWorkKey(values ...string) string {
	keys := taskWorkKeys(values...)
	if len(keys) != 1 {
		return ""
	}
	for key := range keys {
		return key
	}
	return ""
}
