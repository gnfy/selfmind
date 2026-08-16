package httpapi

import (
	"context"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/memory"
)

const recallOutputOverlapThreshold = 0.18

type recallOutputOverlapStats struct {
	Selected       int
	Overlapping    int
	Sources        map[string]int
	OverlapSources map[string]int
	Refs           []string
}

// recallOutputOverlap is a conservative, model-free observability signal. It
// answers only whether the final response reused distinctive language from a
// selected recall slice; it does not claim the slice caused the decision.
func recallOutputOverlap(content string, slices []kernel.RecallSlice) recallOutputOverlapStats {
	stats := recallOutputOverlapStats{
		Sources:        make(map[string]int),
		OverlapSources: make(map[string]int),
	}
	output := memory.BuildSimilaritySignature(strings.TrimSpace(content))
	for _, slice := range slices {
		if strings.TrimSpace(slice.Excerpt) == "" {
			continue
		}
		stats.Selected++
		stats.Sources[slice.Source]++
		score := memory.SignatureSimilarity(output, memory.BuildSimilaritySignature(slice.Excerpt))
		if score < recallOutputOverlapThreshold {
			continue
		}
		stats.Overlapping++
		stats.OverlapSources[slice.Source]++
		if ref := strings.TrimSpace(slice.Ref); ref != "" {
			stats.Refs = append(stats.Refs, ref)
		}
	}
	return stats
}

func (c *RunCoordinator) recordRecallOutputOverlap(ctx context.Context, task *control.Task, run *control.Run, channel, content string) {
	if c == nil || c.srv == nil || c.srv.Control == nil || task == nil || run == nil {
		return
	}
	runtime, ok := kernel.TaskRuntimeContextFromContext(ctx)
	if !ok || len(runtime.RecallSlices) == 0 || strings.TrimSpace(content) == "" {
		return
	}
	stats := recallOutputOverlap(content, runtime.RecallSlices)
	_, _ = c.srv.Control.AppendEvent(context.WithoutCancel(ctx), control.Event{
		TaskID:     task.ID,
		RunID:      run.ID,
		Type:       "context.recall_usage",
		Visibility: "task",
		Channel:    channel,
		Payload: mustJSON(map[string]interface{}{
			"selected":        stats.Selected,
			"output_overlap":  stats.Overlapping,
			"sources":         stats.Sources,
			"overlap_sources": stats.OverlapSources,
			"refs":            stats.Refs,
			"method":          "lexical_overlap_v1",
		}),
	})
}
