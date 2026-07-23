package httpapi

import (
	"regexp"
	"strings"

	"selfmind/internal/control"
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

func taskContainsWorkKey(task control.Task, key string) bool {
	if key == "" {
		return false
	}
	_, ok := taskWorkKeys(task.Title, task.CurrentSummary)[strings.ToUpper(key)]
	return ok
}

// exactWorkKeyCandidate returns a deterministic MOVE target only when exactly
// one offered open label carries the key. Duplicate labels remain a harmless
// display issue instead of becoming an automatic attachment guess.
func exactWorkKeyCandidate(candidates []control.Task, key string) *control.Task {
	var target *control.Task
	for i := range candidates {
		if !taskContainsWorkKey(candidates[i], key) {
			continue
		}
		if target != nil {
			return nil
		}
		target = &candidates[i]
	}
	return target
}
