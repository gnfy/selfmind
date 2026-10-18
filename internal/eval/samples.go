package eval

import (
	"fmt"
	"sort"
	"time"
)

// Repeated recordings must not overwrite earlier attempts. Replay intentionally
// reuses the canonical trajectory; new record samples remain separately named.
func caseSessionID(id string, sample, total int, recording bool) string {
	if recording && total > 1 {
		return fmt.Sprintf("%s-s%d", id, sample+1)
	}
	return id
}

// SamplesOf preserves every attempt, including infrastructure errors. Aggregate
// counters are totals; comparisons use individual samples, never the last one.
func SamplesOf(r RunResult) []RunResult {
	if len(r.Samples) != 0 {
		return r.Samples
	}
	return []RunResult{r}
}

func aggregateSamples(id string, samples []RunResult, threshold float64) *RunResult {
	if threshold <= 0 {
		threshold = 1
	}
	r := &RunResult{CaseID: id, Status: "failed", Samples: samples}
	passed := 0
	for _, s := range samples {
		if s.Status == "passed" {
			passed++
		}
		r.Duration += s.Duration
		r.ToolCalls += s.ToolCalls
		r.ActionToolCalls += s.ActionToolCalls
		r.ProgressUpdates += s.ProgressUpdates
		r.ToolErrors += s.ToolErrors
		r.InputTokens += s.InputTokens
		r.OutputTokens += s.OutputTokens
	}
	rate := float64(passed) / float64(len(samples))
	if rate >= threshold {
		r.Status = "passed"
	}
	r.Checks = []CheckResult{{Name: fmt.Sprintf("pass_rate>=%.2f", threshold), OK: rate >= threshold,
		Message: fmt.Sprintf("%d/%d samples passed", passed, len(samples)), Score: rate}}
	return r
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	x := append([]float64(nil), values...)
	sort.Float64s(x)
	i := len(x) / 2
	if len(x)%2 != 0 {
		return x[i]
	}
	return x[i-1]/2 + x[i]/2
}

func medianDuration(samples []RunResult) time.Duration {
	var values []float64
	for _, s := range samples {
		values = append(values, float64(s.Duration))
	}
	return time.Duration(median(values))
}
