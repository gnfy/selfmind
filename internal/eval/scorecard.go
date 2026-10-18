package eval

import (
	"fmt"
	"selfmind/internal/tools"
	"sort"
	"strings"
	"time"
)

func (s ResultSet) groups() map[string][]RunResult {
	groups := map[string][]RunResult{}
	for _, sample := range s.Samples {
		groups[sample.CaseID] = append(groups[sample.CaseID], sample)
	}
	return groups
}

func (s ResultSet) Markdown() string {
	var b strings.Builder
	b.WriteString("# SelfMind eval scorecard\n\nEvery sample is retained. Durations and Main calls are sample medians; tool errors are totals; replay is not live latency.\n\n")
	b.WriteString("| Case | Passed / samples | Duration | Main calls | Lifecycle calls | Tool errors (total) | Finish first receipt | Closing model time |\n|---|---|---|---|---|---|---|---|\n")
	groups := s.groups()
	for _, id := range sortedCaseIDs(groups) {
		samples := groups[id]
		var life []float64
		errorsTotal := 0
		first, finish := 0, 0
		finishIdentityMissing := false
		for _, sample := range samples {
			errorsTotal += sample.ToolErrors
			if sample.Efficiency != nil {
				life = append(life, float64(sample.Efficiency.LifecycleCalls))
				first += sample.Efficiency.FinishFirstPassRuns
				finish += sample.Efficiency.FinishRuns
				finishIdentityMissing = finishIdentityMissing || sample.Efficiency.FinishIdentityMissing
			}
		}
		finishText := "n/a"
		if finish > 0 && !finishIdentityMissing {
			finishText = fmt.Sprintf("%d/%d", first, finish)
		}
		fmt.Fprintf(&b, "| %s | %d/%d | %s | %s | %s | %d | %s | %s |\n", id, passedSamples(samples), len(samples),
			medianDuration(samples).Round(time.Millisecond), mainCallsText(samples), knownMedian(life, len(samples)), errorsTotal, finishText, closingText(samples))
	}
	for _, id := range sortedCaseIDs(groups) {
		for i, sample := range groups[id] {
			if sample.Provenance != nil {
				p := sample.Provenance
				fmt.Fprintf(&b, "\n%s sample %d: %s; configured route=%s/%s; reasoning=%q; approval=%s; build=%s.\n", id, i+1, p.Mode, p.Provider, p.Model, p.Reasoning, p.ApprovalMode, p.Build)
				if p.Mode == "replay" {
					fmt.Fprintf(&b, "Recording sources: %s (independent of playback configuration).\n", p.RecordingSources)
				}
			}
			if sample.Error != "" {
				fmt.Fprintf(&b, "\n%s sample %d error: %s\n", id, i+1, tools.RedactSensitive(sample.Error))
			}
			for _, check := range sample.Checks {
				if !check.OK {
					fmt.Fprintf(&b, "\n%s sample %d failed check: %s\n", id, i+1, check.Name)
				}
			}
		}
	}
	return b.String()
}

func sortedCaseIDs(groups map[string][]RunResult) []string {
	var keys []string
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func passedSamples(samples []RunResult) int {
	n := 0
	for _, s := range samples {
		if s.Status == "passed" {
			n++
		}
	}
	return n
}

func mainCallsText(samples []RunResult) string {
	var values []float64
	for _, s := range samples {
		if s.Efficiency != nil && s.Efficiency.UsageAvailable && s.Efficiency.UnknownRoleCalls == 0 {
			values = append(values, float64(s.Efficiency.MainCalls))
		}
	}
	return knownMedian(values, len(samples))
}

func knownMedian(values []float64, total int) string {
	if total == 0 || len(values) != total {
		return fmt.Sprintf("n/a (%d/%d known)", len(values), total)
	}
	return fmt.Sprintf("%.1f", median(values))
}

func closingText(samples []RunResult) string {
	var values []float64
	for _, s := range samples {
		if s.Efficiency != nil && s.Efficiency.ClosingAvailable && !s.Efficiency.ClosingIncomplete {
			values = append(values, float64(s.Efficiency.ClosingModelMS))
		}
	}
	if len(values) == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%s (%d/%d measured)", time.Duration(median(values))*time.Millisecond, len(values), len(samples))
}

func efficiencyText(e *Efficiency) string {
	main := "unknown"
	if e.UsageAvailable {
		main = fmt.Sprintf("%d calls / %dms / %d output tokens", e.MainCalls, e.MainMS, e.MainOutputTokens)
	}
	return fmt.Sprintf("Efficiency: Main %s; lifecycle=%d verify=%d; finish first receipt=%d/%d; closing=%dms (measured=%t)",
		main, e.LifecycleCalls, e.VerifyCalls, e.FinishFirstPassRuns, e.FinishRuns, e.ClosingModelMS, e.ClosingAvailable)
}

// CompareResultSets is advisory. It never turns a speed change into a quality
// verdict, and refuses latency deltas across modes, routes or changed fixtures.
func CompareResultSets(before, after ResultSet) string {
	left, right := before.groups(), after.groups()
	all := map[string][]RunResult{}
	for id := range left {
		all[id] = nil
	}
	for id := range right {
		all[id] = nil
	}
	var b strings.Builder
	b.WriteString("# Eval comparison (advisory)\n\nAll attempts, including failures, enter the medians. Compare quality first: faster failed work is not an improvement. No new release gate.\n\n| Case | Before passed | After passed | Duration before → after | Delta | Comparability |\n|---|---|---|---|---|---|\n")
	var details strings.Builder
	for _, id := range sortedCaseIDs(all) {
		a, z := left[id], right[id]
		reason := comparisonIdentity(a, z)
		delta := "n/a"
		if reason == "matched" && medianDuration(a) > 0 {
			delta = fmt.Sprintf("%+.1f%%", 100*(float64(medianDuration(z))/float64(medianDuration(a))-1))
		}
		fmt.Fprintf(&b, "| %s | %d/%d | %d/%d | %s → %s | %s | %s |\n", id, passedSamples(a), len(a), passedSamples(z), len(z),
			medianDuration(a).Round(time.Millisecond), medianDuration(z).Round(time.Millisecond), delta, reason)
		fmt.Fprintf(&details, "\n%s: Main calls %s → %s; closing %s → %s.\n\n", id, mainCallsText(a), mainCallsText(z), closingText(a), closingText(z))
	}
	b.WriteString(details.String())
	return b.String()
}

func comparisonIdentity(a, b []RunResult) string {
	if len(a) == 0 || len(b) == 0 {
		return "unmatched case"
	}
	if len(a) != len(b) {
		return "sample counts differ; no paired speed claim"
	}
	if a[0].Provenance == nil {
		return "unknown provenance"
	}
	expected := *a[0].Provenance
	expected.Build = ""
	expected.RecordingSession = ""
	if expected.CaseHash == "" || expected.PolicyHash == "" || expected.Provider == "" || expected.Model == "" || expected.Mode == "" {
		return "unknown provenance"
	}
	for _, samples := range [][]RunResult{a, b} {
		for _, s := range samples {
			if s.Status == "error" || s.Status == "incomplete" {
				return "unfinished or infrastructure error; no speed claim"
			}
			if s.Provenance == nil {
				return "unknown provenance"
			}
			p := *s.Provenance
			p.Build = ""
			p.RecordingSession = ""
			if p != expected {
				return "mode, route, policy or fixture differs"
			}
		}
	}
	if expected.Mode == "replay" {
		return "replay only; no live speed claim"
	}
	return "matched"
}
