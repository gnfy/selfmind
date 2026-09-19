// Package verification projects execution evidence without interpreting goals.
package verification

import (
	"fmt"
	"sort"
	"strings"
)

// Binding is a model-declared proof obligation, not a claim that it is met.
// Version zero has historical command identity and no replacement authority.
type Binding struct {
	Version   int    `json:"version"`
	Criterion string `json:"criterion"`
	Target    string `json:"target"`
	Replaces  string `json:"replaces,omitempty"`
	Reason    string `json:"reason,omitempty"`
	// Version 3 binds an explicitly selected, server-issued plan step.
	StepID string `json:"step_id,omitempty"`
	// Nil keeps the historical whole-run invalidation rule. Version 2 makes
	// Main's local dependency declaration explicit; an empty list means the
	// criterion does not depend on local files. This is not execution authority.
	LocalDependencies *[]string `json:"local_dependencies,omitempty"`
	// Runtime-observed referents at check time. These may change on a recheck
	// without changing Main's declared input paths (for example a retargeted link).
	ResolvedLocalDependencies []string `json:"resolved_local_dependencies,omitempty"`
	// Missing or unreadable declarations cannot prove independence from other
	// file changes. Retain the declaration for diagnosis and use conservative invalidation.
	UnresolvedLocalDependencies []string `json:"unresolved_local_dependencies,omitempty"`
}

type Check struct {
	ToolCallID string   `json:"tool_call_id,omitempty"`
	Kind       string   `json:"kind,omitempty"`
	Command    string   `json:"command,omitempty"`
	CWD        string   `json:"cwd,omitempty"`
	Status     string   `json:"status"`
	ExitCode   int      `json:"exit_code"`
	StartedAt  int64    `json:"started_at_unix_nano,omitempty"`
	FinishedAt int64    `json:"finished_at_unix_nano,omitempty"`
	Binding    *Binding `json:"binding,omitempty"`
}

func ValidBinding(b *Binding) bool {
	return b != nil && (b.Version == 1 || (b.Version == 2 && b.LocalDependencies != nil) || (b.Version == 3 && b.StepID != "")) && strings.TrimSpace(b.Criterion) != "" && strings.TrimSpace(b.Target) != ""
}

// CanReplace checks reference integrity, not semantic equivalence. Main must
// explain why its corrected method still proves the unchanged obligation.
func CanReplace(old, next Check) bool {
	return ValidBinding(old.Binding) && ValidBinding(next.Binding) &&
		old.ToolCallID != "" && next.Binding.Replaces == old.ToolCallID && strings.TrimSpace(next.Binding.Reason) != "" &&
		old.Binding.Criterion == next.Binding.Criterion && old.Binding.Target == next.Binding.Target && old.CWD == next.CWD &&
		old.Binding.StepID == next.Binding.StepID &&
		sameDependencies(old.Binding, next.Binding) &&
		old.FinishedAt <= next.StartedAt && old.ToolCallID != next.ToolCallID
}

func Latest(checks []Check) []Check {
	ordered := append([]Check(nil), checks...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].StartedAt < ordered[j].StartedAt })
	replaced := map[string]bool{}
	byID := map[string]Check{}
	for _, c := range ordered {
		if c.Binding != nil && c.Binding.Replaces != "" {
			if old, ok := byID[c.Binding.Replaces]; ok && CanReplace(old, c) {
				replaced[old.ToolCallID] = true
			}
		}
		if c.ToolCallID != "" {
			byID[c.ToolCallID] = c
		}
	}
	latest := map[string]Check{}
	keys := []string{}
	for _, c := range ordered {
		if replaced[c.ToolCallID] {
			continue
		}
		key := strings.TrimSpace(c.Kind) + "\x00" + strings.TrimSpace(c.Command) + "\x00" + strings.TrimSpace(c.CWD)
		if c.Binding != nil {
			// Bound attempts only supersede by explicit reference. This also keeps an
			// invalid/future binding from inheriting legacy command-based replacement.
			key += fmt.Sprintf("\x00%d\x00%s\x00%s\x00%s\x00%d", c.Binding.Version, c.Binding.Criterion, c.Binding.Target, c.ToolCallID, c.StartedAt)
		}
		old, ok := latest[key]
		if !ok {
			keys = append(keys, key)
		}
		if !ok || c.FinishedAt >= old.FinishedAt {
			latest[key] = c
		}
	}
	out := make([]Check, 0, len(keys))
	for _, key := range keys {
		out = append(out, latest[key])
	}
	return out
}

func State(latestMutation int64, checks []Check) (string, string) {
	return state(latestMutation, checks, func(Check) int64 { return latestMutation })
}

func state(latestMutation int64, checks []Check, affectedAt func(Check) int64) (string, string) {
	if len(checks) == 0 {
		if latestMutation == 0 {
			return "not_applicable", "No code mutation or verification was recorded."
		}
		return "not_run", "Files changed, but no verification command was recorded after the change."
	}
	current := []Check{}
	stale := 0
	for _, c := range Latest(checks) {
		if c.StartedAt >= affectedAt(c) {
			current = append(current, c)
		} else {
			stale++
		}
	}
	if len(current) == 0 {
		return "stale", "Verification exists, but it ran before the latest file change."
	}
	passed, failed, blocked := 0, 0, 0
	for _, c := range Latest(current) {
		switch c.Status {
		case "succeeded":
			passed++
		case "blocked":
			blocked++
		default:
			failed++
		}
	}
	switch {
	case failed > 0:
		return "failed", fmt.Sprintf("%d current verification check(s) failed.", failed)
	case stale > 0:
		return "stale", fmt.Sprintf("%d verification check(s) need review after changes to their inputs.", stale)
	case passed > 0 && blocked == 0:
		return "passed", fmt.Sprintf("%d current verification check(s) passed.", passed)
	case passed == 0 && blocked > 0:
		return "blocked", fmt.Sprintf("%d verification check(s) were blocked.", blocked)
	default:
		return "partial", fmt.Sprintf("%d verification check(s) passed and %d were blocked.", passed, blocked)
	}
}
