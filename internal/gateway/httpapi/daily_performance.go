package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"

	"selfmind/internal/control"
)

type dailyCacheSample struct {
	Calls, Unavailable int
	Input, Read        int64
}
type dailyPerformance struct {
	PreparationSamples    int
	PreparationMS         int64
	RecallMS, ExpansionMS int64
	ExpansionStatuses     map[string]int
	First, Later          dailyCacheSample
	BookkeepingSteps      int
}

// Measurements remain separate: tool durations can include human/model waits,
// and absent historical markers cannot be inferred from an iteration number.
func collectDailyPerformance(events []control.Event) dailyPerformance {
	stats := dailyPerformance{ExpansionStatuses: map[string]int{}}
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
				Duration    int64  `json:"elapsed_ms"`
				ExpansionMS int64  `json:"expansion_ms"`
				Status      string `json:"expansion_status"`
			}
			if json.Unmarshal(event.Payload, &p) == nil {
				stats.RecallMS += p.Duration
				stats.ExpansionMS += p.ExpansionMS
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
			}
			if json.Unmarshal(event.Payload, &p) != nil || p.First == nil || p.Status == "deferred" && p.Dispatched != nil && !*p.Dispatched {
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
		case "agent.step":
			var p struct{ Outcome, Detail string }
			if json.Unmarshal(event.Payload, &p) != nil || p.Outcome != "execute_tools" || p.Detail == "" {
				continue
			}
			bookkeeping := true
			for _, name := range strings.Split(p.Detail, ",") {
				if name != "update_plan" && name != "finish_run" {
					bookkeeping = false
				}
			}
			if bookkeeping {
				stats.BookkeepingSteps++
			}
		}
	}
	return stats
}

func formatCacheSample(sample dailyCacheSample) string {
	if sample.Calls == 0 {
		return fmt.Sprintf("unavailable (%d unreported)", sample.Unavailable)
	}
	return fmt.Sprintf("%d%% (%d reported, %d unreported)", sample.Read*100/sample.Input, sample.Calls, sample.Unavailable)
}

func (p dailyPerformance) render() string {
	return fmt.Sprintf("Preparation: %d measured runs, total %dms (excludes queued time); recall %dms including expansion %dms (%s)\nCache by actual request position: first %s; later %s (historical missing markers excluded)\nBookkeeping-only model steps: %d (update_plan/finish_run; Skill reads are not bookkeeping)\n",
		p.PreparationSamples, p.PreparationMS, p.RecallMS, p.ExpansionMS, formatCountMap(p.ExpansionStatuses), formatCacheSample(p.First), formatCacheSample(p.Later), p.BookkeepingSteps)
}
