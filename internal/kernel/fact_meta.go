package kernel

import (
	"context"

	"selfmind/internal/kernel/memory"
)

// factWithMeta builds a fact carrying governance metadata (W3): source, scope
// (derived from the target + active workspace), a base confidence for the
// source, and the run it was extracted from. Used by the extractors so durable
// memory records where it came from, how much to trust it, and where it applies.
func factWithMeta(ctx context.Context, target, content, source string) memory.Fact {
	workspaceID := ""
	if ws, ok := WorkspaceContextFromContext(ctx); ok {
		workspaceID = ws.ID
	}
	runID := ""
	if rt, ok := TaskRuntimeContextFromContext(ctx); ok {
		runID = rt.RunID
	}
	return memory.Fact{
		Target:         target,
		Content:        content,
		Source:         source,
		Scope:          memory.DeriveFactScope(target, workspaceID),
		Confidence:     memory.BaseConfidence(source),
		CreatedFromRun: runID,
	}
}
