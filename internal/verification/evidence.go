// Package verification projects execution evidence without interpreting goals.
package verification

import (
	"fmt"
	"sort"
	"strings"

	"selfmind/internal/executionenv"
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
	// Original observation kind remains visible after explicit Main adoption.
	Observation    *Observation `json:"observation,omitempty"`
	ToolCallID     string       `json:"tool_call_id,omitempty"`
	Kind           string       `json:"kind,omitempty"`
	Command        string       `json:"command,omitempty"`
	CWD            string       `json:"cwd,omitempty"`
	Status         string       `json:"status"`
	ExitCode       int          `json:"exit_code"`
	StartedAt      int64        `json:"started_at_unix_nano,omitempty"`
	FinishedAt     int64        `json:"finished_at_unix_nano,omitempty"`
	Binding        *Binding     `json:"binding,omitempty"`
	ProcessStarted *bool        `json:"process_started,omitempty"`
	Invoked        *bool        `json:"invoked,omitempty"`
	EffectState    string       `json:"effect_state,omitempty"`
	// NetworkDecision and NetworkPermission are what the runtime recorded about
	// the network for this check's process (see executionenv.NetworkDecision*).
	// Evidence written before they existed carries neither, and says nothing.
	NetworkDecision   string `json:"network_decision,omitempty"`
	NetworkPermission string `json:"network_permission,omitempty"`
	// NetworkMayFetch: the command can fetch dependencies when it runs (a build
	// or a test, not a vet or a formatter). It is set only on a check that ran
	// offline despite a grant, and says whether that is worth telling anyone.
	NetworkMayFetch bool `json:"network_may_fetch,omitempty"`
	// Source identifies an explicitly adopted prior observation. Its original
	// call id and timestamps stay intact; it is not an execution in this Run.
	Source *CheckSource `json:"source,omitempty"`
}

type CheckSource struct {
	RunID       string `json:"run_id"`
	StepID      string `json:"step_id"`
	PlanVersion int    `json:"plan_version"`
	Reason      string `json:"reason"`
}

// WithDispatchFacts preserves preparation refusals and unknown process exits
// independently of the tool wrapper's reported status. Nil process facts keep
// the historical compatibility rule, never inventing a new observed start.
func WithDispatchFacts(check Check, processExit *int) Check {
	if check.ProcessStarted != nil {
		if !*check.ProcessStarted || processExit == nil {
			check.Status = "blocked"
		} else {
			check.ExitCode = *processExit
			if check.ExitCode != 0 {
				check.Status = "failed"
			}
		}
	}
	if (check.Invoked != nil && !*check.Invoked) || check.EffectState == "not_dispatched" ||
		(check.ProcessStarted == nil && (check.Invoked != nil || check.EffectState != "")) {
		check.Status = "blocked"
	}
	return check
}

// offlineCoverageLost reports a passing attempt whose coverage the missing
// network may have reduced: it ran offline although a grant permits the network,
// and its command can fetch dependencies.
func offlineCoverageLost(c Check) bool {
	return c.NetworkMayFetch && executionenv.NetworkOfflineDespiteGrant(c.NetworkDecision, c.NetworkPermission)
}

// OfflineDespiteGrant counts the effective passing attempts that ran without
// network although a network grant permits it, and whose command can fetch
// dependencies. A pass is evidence of what the check exercised; for such an
// attempt that excludes anything needing a remote service, and the summaries
// say so instead of letting a bare "passed" claim more. A failing or blocked
// attempt reports its own failure instead, and a check that cannot fetch
// anything has no coverage to lose.
func OfflineDespiteGrant(checks []Check) int {
	offline := 0
	for _, c := range Latest(checks) {
		if c.Status == "succeeded" && offlineCoverageLost(c) {
			offline++
		}
	}
	return offline
}

// offlineCoverageNote is the sentence appended to a passing summary.
func offlineCoverageNote(offline int) string {
	if offline == 0 {
		return ""
	}
	return fmt.Sprintf(" %d of them ran without network although this workspace's network grant permits it: coverage that needs remote data was not exercised; re-run with network: true if the criterion needs it.", offline)
}

func ValidBinding(b *Binding) bool {
	return b != nil && (b.Version == 1 || (b.Version == 2 && b.LocalDependencies != nil) || (b.Version == 3 && b.StepID != "")) && strings.TrimSpace(b.Criterion) != "" && strings.TrimSpace(b.Target) != ""
}

// CanReplace checks reference integrity, not semantic equivalence. Main must
// explain why its corrected method still proves the unchanged obligation.
func CanReplace(old, next Check) bool {
	return ReplacementMismatch(old, next) == ""
}

// ReplacementMismatch explains the runtime-owned part of replacement
// identity. A plan step is an execution association and local dependencies
// describe the chosen observation method. Both may change while Main preserves
// the criterion and target and explains the replacement.
func ReplacementMismatch(old, next Check) string {
	if !ValidBinding(old.Binding) {
		return "referenced evidence has no valid proof obligation"
	}
	if !ValidBinding(next.Binding) {
		return "new evidence has no valid proof obligation"
	}
	if old.ToolCallID == "" || next.Binding.Replaces != old.ToolCallID {
		return "replacement reference does not identify the prior evidence"
	}
	if strings.TrimSpace(next.Binding.Reason) == "" {
		return "replacement reason is empty"
	}
	if old.Binding.Criterion != next.Binding.Criterion {
		return fmt.Sprintf("criterion changed from %q to %q", old.Binding.Criterion, next.Binding.Criterion)
	}
	if old.Binding.Target != next.Binding.Target {
		return fmt.Sprintf("target changed from %q to %q", old.Binding.Target, next.Binding.Target)
	}
	if old.CWD != next.CWD {
		return fmt.Sprintf("working directory changed from %q to %q", old.CWD, next.CWD)
	}
	if old.FinishedAt > next.StartedAt {
		return "replacement started before the prior attempt finished"
	}
	if old.ToolCallID == next.ToolCallID {
		return "replacement reused the prior evidence id"
	}
	return ""
}

func Latest(checks []Check) []Check {
	ordered := append([]Check(nil), checks...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].StartedAt < ordered[j].StartedAt })
	replaced := map[string]bool{}
	byID := map[string]Check{}
	for _, c := range ordered {
		if c.Binding != nil && c.Binding.Replaces != "" {
			if old, ok := byID[checkReference(c, c.Binding.Replaces)]; ok && CanReplace(old, c) {
				replaced[checkReference(old, old.ToolCallID)] = true
			}
		}
		if c.ToolCallID != "" {
			byID[checkReference(c, c.ToolCallID)] = c
		}
	}
	latest := map[string]Check{}
	seenKeys := map[string]bool{}
	keys := []string{}
	for _, c := range ordered {
		if replaced[checkReference(c, c.ToolCallID)] {
			continue
		}
		key := strings.TrimSpace(c.Kind) + "\x00" + strings.TrimSpace(c.Command) + "\x00" + strings.TrimSpace(c.CWD)
		if ValidBinding(c.Binding) && c.Binding.Version == 3 {
			// A plan step owns one durable acceptance obligation. Commands are
			// observation methods, so a later attempt against the exact same
			// runtime-owned step, criterion, target, cwd, and declared inputs is
			// the effective state. Every attempt remains in the event history.
			dependencies := "<all-local-inputs>"
			if c.Binding.LocalDependencies != nil {
				dependencies = strings.Join(*c.Binding.LocalDependencies, "\x1f")
			}
			key = fmt.Sprintf("plan-step\x00%s\x00%s\x00%s\x00%s\x00%s", c.Binding.StepID, c.Binding.Criterion, c.Binding.Target, strings.TrimSpace(c.CWD), dependencies)
			if c.Status == "succeeded" {
				for existingKey, existing := range latest {
					existingDeps := "<all-local-inputs>"
					if existing.Binding != nil && existing.Binding.LocalDependencies != nil {
						existingDeps = strings.Join(*existing.Binding.LocalDependencies, "\x1f")
					}
					unresolvedFixed := c.Binding.StepID != "" &&
						len(existing.Binding.UnresolvedLocalDependencies) > 0 &&
						len(c.Binding.UnresolvedLocalDependencies) == 0 &&
						(existing.Binding.Target == c.Binding.Target || existing.Binding.Target == existing.Binding.StepID || c.Binding.Target == c.Binding.StepID)
					if existingKey != key &&
						ValidBinding(existing.Binding) && existing.Binding.Version == 3 &&
						existing.Binding.StepID == c.Binding.StepID &&
						sameAssessmentSource(existing.Source, c.Source) &&
						existing.FinishedAt > 0 && existing.FinishedAt <= c.StartedAt &&
						(unresolvedFixed ||
							(existing.Binding.Criterion == c.Binding.Criterion &&
								(existing.Binding.Target == c.Binding.Target || (existing.Status != "succeeded" && c.Binding.StepID != "" && targetCovers(c.Binding.Target, existing.Binding.Target))) &&
								(existing.Status != "succeeded" || (c.Binding.StepID != "" && existingDeps == dependencies)))) {
						delete(latest, existingKey)
					}
				}
			}
		} else if c.Binding != nil {
			// Bound attempts only supersede by explicit reference. This also keeps an
			// invalid/future binding from inheriting legacy command-based replacement.
			key += fmt.Sprintf("\x00%d\x00%s\x00%s\x00%s\x00%d", c.Binding.Version, c.Binding.Criterion, c.Binding.Target, c.ToolCallID, c.StartedAt)
		}
		old, ok := latest[key]
		if !seenKeys[key] {
			seenKeys[key] = true
			keys = append(keys, key)
		}
		if !ok || c.FinishedAt >= old.FinishedAt {
			latest[key] = c
		}
	}
	out := make([]Check, 0, len(keys))
	for _, key := range keys {
		if c, ok := latest[key]; ok {
			out = append(out, c)
		}
	}
	return out
}

func checkReference(check Check, id string) string {
	if check.Source != nil {
		return check.Source.RunID + "\x00" + id
	}
	return "\x00" + id
}

func targetCovers(broad, narrow string) bool {
	broad = strings.TrimSpace(broad)
	narrow = strings.TrimSpace(narrow)
	if broad == "" || narrow == "" {
		return false
	}
	if broad == narrow {
		return true
	}
	for _, part := range strings.FieldsFunc(broad, func(r rune) bool {
		return r == '+' || r == ',' || r == ';'
	}) {
		if strings.TrimSpace(part) == narrow {
			return true
		}
	}
	return false
}

func State(latestMutation int64, checks []Check) (string, string) {
	return state(latestMutation, checks, func(Check) int64 { return latestMutation })
}

// stepCheckSupersededByFreshPass reports whether an earlier check on a Plan
// step has been superseded by a subsequent fresh passing check on the same
// step and source.
func stepCheckSupersededByFreshPass(c Check, latest []Check, affectedAt func(Check) int64) bool {
	if !ValidBinding(c.Binding) || c.Binding.Version != 3 || c.Binding.StepID == "" || c.FinishedAt <= 0 {
		return false
	}
	for _, other := range latest {
		if other.ToolCallID == c.ToolCallID || !ValidBinding(other.Binding) || other.Binding.Version != 3 ||
			other.Binding.StepID != c.Binding.StepID || !sameAssessmentSource(other.Source, c.Source) {
			continue
		}
		if other.Status == "succeeded" && other.StartedAt >= c.FinishedAt && other.StartedAt >= affectedAt(other) {
			return true
		}
	}
	return false
}

func state(latestMutation int64, checks []Check, affectedAt func(Check) int64) (string, string) {
	if len(checks) == 0 {
		if latestMutation == 0 {
			return "not_applicable", "No code mutation or verification was recorded."
		}
		return "not_run", "Files changed, but no verification command was recorded after the change."
	}
	latest := Latest(checks)
	current := []Check{}
	stale := 0
	for _, c := range latest {
		if c.StartedAt >= affectedAt(c) {
			current = append(current, c)
		} else if !stepCheckSupersededByFreshPass(c, latest, affectedAt) {
			stale++
		}
	}
	if len(current) == 0 {
		return "stale", "Verification exists, but it ran before the latest file change."
	}
	passed, failed, blocked, offline := 0, 0, 0, 0
	for _, c := range Latest(current) {
		switch c.Status {
		case "succeeded":
			passed++
			if offlineCoverageLost(c) {
				offline++
			}
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
		return "passed", fmt.Sprintf("%d current verification check(s) passed.", passed) + offlineCoverageNote(offline)
	case passed == 0 && blocked > 0:
		return "blocked", fmt.Sprintf("%d verification check(s) were blocked.", blocked)
	default:
		return "partial", fmt.Sprintf("%d verification check(s) passed and %d were blocked.", passed, blocked) + offlineCoverageNote(offline)
	}
}
