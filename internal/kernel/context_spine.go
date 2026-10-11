package kernel

import (
	"context"
	"encoding/json"

	"selfmind/internal/kernel/llm"
)

// Fold only recorded resume lineage, never a workspace, Thread label, work
// key, or similarity guess. Full stored records remain available to search.
func boundedHistoryMessagesForContext(ctx context.Context, blobs [][]byte) []llm.Message {
	runtime, _ := TaskRuntimeContextFromContext(ctx)
	lineage := make(map[string]bool, len(runtime.SpineLineageRunIDs))
	for _, id := range runtime.SpineLineageRunIDs {
		lineage[id] = true
	}
	seen := make(map[string]int)
	selected := make([][]byte, 0, composerSpineTailEntries)
	for _, blob := range blobs { // latest first: the last result represents its chain
		entry, ok := parseSpineEntry(blob)
		if !ok {
			selected = append(selected, blob)
			continue
		}
		chain := entry.ChainID
		if lineage[entry.RunID] {
			chain = runtime.SpineChainID
		}
		if chain == "" {
			chain = entry.RunID
		} // unknown legacy edges remain distinct
		if index, found := seen[chain]; chain != "" && found {
			latest, _ := parseSpineEntry(selected[index])
			latest.FoldedTurns++
			if entry.User != "" {
				latest.EarlierUser, latest.EarlierObservedAt = entry.User, entry.ObservedAt
			}
			selected[index], _ = json.Marshal(latest)
			continue
		}
		if chain != "" {
			seen[chain] = len(selected)
		}
		selected = append(selected, blob)
		if len(selected) >= composerSpineTailEntries {
			break
		}
	}
	return boundedHistoryMessages(selected)
}
