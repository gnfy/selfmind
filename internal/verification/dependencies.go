package verification

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// Mutation contains runtime-observed changes, including partial failed writes.
type Mutation struct {
	Path       string
	FinishedAt int64
}

func sameDependencies(a, b *Binding) bool {
	if a.Version != b.Version {
		return false
	}
	if a.LocalDependencies == nil || b.LocalDependencies == nil {
		return a.LocalDependencies == nil && b.LocalDependencies == nil
	}
	return slices.Equal(*a.LocalDependencies, *b.LocalDependencies)
}

func RelevantMutationAt(check Check, mutations []Mutation) int64 {
	latest := int64(0)
	for _, mutation := range mutations {
		affected := true
		if ValidBinding(check.Binding) && check.Binding.Version >= 2 && check.Binding.LocalDependencies != nil && len(check.Binding.UnresolvedLocalDependencies) == 0 && filepath.IsAbs(mutation.Path) {
			affected = false
			dependencies := append(append([]string(nil), (*check.Binding.LocalDependencies)...), check.Binding.ResolvedLocalDependencies...)
			for _, dependency := range dependencies {
				// Missing/invalid persisted input identities remain conservative.
				if !filepath.IsAbs(dependency) || overlapsPath(dependency, mutation.Path) {
					affected = true
					break
				}
			}
		}
		if affected && mutation.FinishedAt > latest {
			latest = mutation.FinishedAt
		}
	}
	return latest
}

func overlapsPath(a, b string) bool {
	contains := func(root, target string) bool {
		rel, err := filepath.Rel(root, target)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	return contains(a, b) || contains(b, a)
}

func StateWithMutations(mutations []Mutation, checks []Check) (string, string) {
	latest := RelevantMutationAt(Check{}, mutations)
	status, summary := state(latest, checks, func(check Check) int64 { return RelevantMutationAt(check, mutations) })
	if status == "stale" {
		var gaps []string
		for _, check := range Latest(checks) {
			if check.StartedAt >= RelevantMutationAt(check, mutations) {
				continue
			}
			target := "undeclared inputs"
			if check.Binding != nil {
				target = check.Binding.Target
			}
			if r := []rune(target); len(r) > 200 {
				target = string(r[:200]) + "…"
			}
			gaps = append(gaps, fmt.Sprintf("%s (target %q)", check.ToolCallID, target))
			if len(gaps) == 4 {
				break
			}
		}
		summary += " Recheck: " + strings.Join(gaps, "; ") + "."
	}
	if status != "passed" {
		summary += Blockers(mutations, checks)
	}
	return status, summary
}
