package httpapi

import (
	"encoding/json"
	"fmt"

	"selfmind/internal/control"
)

type dailyCacheSample struct {
	Calls, Unavailable int
	Input, Read        int64
}
type dailyPerformance struct {
	PreparationSamples             int
	PreparationMS                  int64
	RecallMS, ExpansionMS          int64
	ExpansionOverlapped            int
	ExpansionStatuses              map[string]int
	First, Later                   dailyCacheSample
	BookkeepingSteps               int
	HeldCalls, UnreportedToolSteps int
	// ModelMS is the Run's own model-call time: the denominator for what its
	// failed steps cost.
	ModelMS      int64
	StepFailures dailyStepFailures
	PlanInput    dailyPlanInput
}

// dailyToolStep is the tool calls one Run's own loop made between two of its
// model calls. The live agent.step event is never stored, so a step is rebuilt
// from durable tool.completed facts: the dispatcher's invoked flag, and the
// loop's plan_boundary_held refusal for a call it never dispatched.
type dailyToolStep struct {
	held       int
	invoked    []string
	unreported bool
	// What the step cost and which of its calls failed (daily_step_failures.go).
	seq      int
	modelMS  int64
	calls    []string
	failures []dailyStepFailure
}

func (s *dailyToolStep) addTo(stats *dailyPerformance) {
	stats.HeldCalls += s.held
	if s.unreported {
		stats.UnreportedToolSteps++
		return
	}
	if len(s.invoked) == 0 {
		return
	}
	for _, name := range s.invoked {
		if name != "update_plan" && name != "finish_run" {
			return
		}
	}
	stats.BookkeepingSteps++
}

// Measurements remain separate: tool durations can include human/model waits,
// and absent historical markers cannot be inferred from an iteration number.
// Sub-agent (delegated) and batch_read child (nested) events interleave with the
// Run's own calls but are not its steps, and an event without a Run has no step.
func collectDailyPerformance(events []control.Event) dailyPerformance {
	stats := dailyPerformance{ExpansionStatuses: map[string]int{}}
	open := map[string]*dailyToolStep{}
	failures, planInput := newDailyStepFailureTracker(), newDailyPlanInputTracker()
	closeStep := func(runID string) {
		if step := open[runID]; step != nil {
			step.addTo(&stats)
			failures.finish(runID, step)
			delete(open, runID)
		}
	}
	for _, event := range events {
		switch event.Type {
		case "run.prepared":
			var p struct {
				Duration int64 `json:"duration_ms"`
			}
			if json.Unmarshal(event.Payload, &p) == nil {
				stats.PreparationSamples++
				stats.PreparationMS += p.Duration
			}
		case "context.recall":
			var p struct {
				Duration            int64  `json:"elapsed_ms"`
				ExpansionMS         int64  `json:"expansion_ms"`
				ExpansionOverlapped bool   `json:"expansion_overlapped"`
				Status              string `json:"expansion_status"`
			}
			if json.Unmarshal(event.Payload, &p) == nil {
				stats.RecallMS += p.Duration
				stats.ExpansionMS += p.ExpansionMS
				if p.ExpansionOverlapped {
					stats.ExpansionOverlapped++
				}
				stats.ExpansionStatuses[fallback(p.Status, "unreported")]++
			}
		case "provider.call.usage":
			var p struct {
				First      *bool  `json:"first_call"`
				Input      int64  `json:"input_tokens"`
				Read       int64  `json:"cache_read_input_tokens"`
				Reported   bool   `json:"cache_usage_reported"`
				Dispatched *bool  `json:"provider_dispatched"`
				Status     string `json:"status"`
				Delegated  bool   `json:"delegated"`
				DurationMS int64  `json:"duration_ms"`
			}
			if json.Unmarshal(event.Payload, &p) != nil {
				continue
			}
			// A typed local deferral made no provider request: it is no model call.
			deferred := p.Status == "deferred" && p.Dispatched != nil && !*p.Dispatched
			if !p.Delegated {
				closeStep(event.RunID)
				if event.RunID != "" && !deferred {
					failures.modelCall(event.RunID, p.DurationMS)
					stats.ModelMS += p.DurationMS
				}
			}
			if p.First == nil || deferred {
				continue
			}
			sample := &stats.Later
			if *p.First {
				sample = &stats.First
			}
			if p.Input <= 0 || !p.Reported {
				sample.Unavailable++
				continue
			}
			sample.Calls++
			sample.Input += p.Input
			sample.Read += p.Read
		case "tool.completed":
			var p struct {
				Tool          string `json:"tool"`
				ErrorCode     string `json:"error_code"`
				Error         string `json:"error"`
				ErrorCategory string `json:"error_category"`
				Invoked       *bool  `json:"invoked"`
				Delegated     bool   `json:"delegated"`
				Nested        bool   `json:"nested"`
			}
			if event.RunID == "" || json.Unmarshal(event.Payload, &p) != nil || p.Delegated || p.Nested {
				continue
			}
			step := open[event.RunID]
			if step == nil {
				step = &dailyToolStep{}
				failures.open(event.RunID, step)
				open[event.RunID] = step
			}
			step.calls = append(step.calls, p.Tool)
			if failure, failed := dailyCallFailure(p.Tool, p.Error, p.ErrorCategory, p.ErrorCode); failed {
				step.failures = append(step.failures, failure)
			}
			if p.ErrorCode == "plan_boundary_held" {
				step.held++
			}
			switch {
			case p.Invoked == nil:
				step.unreported = true
			case *p.Invoked:
				step.invoked = append(step.invoked, p.Tool)
			}
		}
		planInput.observe(event)
	}
	for runID := range open {
		closeStep(runID)
	}
	stats.StepFailures, stats.PlanInput = failures.dailyStepFailures, planInput.dailyPlanInput
	return stats
}

func formatCacheSample(sample dailyCacheSample) string {
	if sample.Calls == 0 {
		return fmt.Sprintf("unavailable (%d unreported)", sample.Unavailable)
	}
	return fmt.Sprintf("%d%% (%d reported, %d unreported)", sample.Read*100/sample.Input, sample.Calls, sample.Unavailable)
}

func (p dailyPerformance) render() string {
	base := fmt.Sprintf("Preparation: %d measured runs, total %dms (excludes queued time); recall %dms including expansion %dms (%s)\nCache by actual request position: first %s; later %s (historical missing markers excluded)\nBookkeeping-only model steps: %d (invoked update_plan/finish_run; Skill reads are not bookkeeping); held calls: %d; dispatch metadata missing: %d steps\n",
		p.PreparationSamples, p.PreparationMS, p.RecallMS, p.ExpansionMS, formatCountMap(p.ExpansionStatuses), formatCacheSample(p.First), formatCacheSample(p.Later), p.BookkeepingSteps, p.HeldCalls, p.UnreportedToolSteps)
	if p.ExpansionOverlapped > 0 {
		base += fmt.Sprintf("Recall expansion overlapped lexical selection in %d run(s); expansion time is work, not additional foreground wait.\n", p.ExpansionOverlapped)
	}
	return base + p.StepFailures.render(p.ModelMS) + p.PlanInput.render()
}
