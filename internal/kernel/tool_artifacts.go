package kernel

import "context"

// ToolArtifactRef identifies one spooled tool output. The id doubles as the
// storage key: the gateway sink names the on-disk file after it, and the
// read-back tool (tools.tool_output_view) resolves it without a DB round trip.
type ToolArtifactRef struct {
	ID    string
	Bytes int
}

// ToolArtifactSink persists the full (capture-capped) output of a tool call
// whose model surface had to be truncated, so the model can later read any
// omitted range by reference instead of re-running the command. Kernel stays
// storage-agnostic: the gateway installs an implementation per run (it owns
// the data dir and the control-plane artifact row); no sink = the envelope
// degrades to the plain head/tail marker. Implementations must be safe for
// concurrent use (read-only tool batches run in parallel).
type ToolArtifactSink interface {
	SaveToolOutput(ctx context.Context, toolName, content string) (ToolArtifactRef, error)
}

type toolArtifactSinkKey struct{}

// WithToolArtifactSink installs the per-run tool-output artifact sink.
func WithToolArtifactSink(ctx context.Context, sink ToolArtifactSink) context.Context {
	if ctx == nil || sink == nil {
		return ctx
	}
	return context.WithValue(ctx, toolArtifactSinkKey{}, sink)
}

// ToolArtifactSinkFromContext returns the run's sink, or nil.
func ToolArtifactSinkFromContext(ctx context.Context) ToolArtifactSink {
	if ctx == nil {
		return nil
	}
	sink, _ := ctx.Value(toolArtifactSinkKey{}).(ToolArtifactSink)
	return sink
}
