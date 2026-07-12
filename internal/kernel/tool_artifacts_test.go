package kernel

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

type fakeArtifactSink struct {
	saved   []string
	nextID  string
	fail    bool
	lastLen int
}

func (f *fakeArtifactSink) SaveToolOutput(_ context.Context, toolName, content string) (ToolArtifactRef, error) {
	if f.fail {
		return ToolArtifactRef{}, fmt.Errorf("spool unavailable")
	}
	f.saved = append(f.saved, toolName)
	f.lastLen = len(content)
	id := f.nextID
	if id == "" {
		id = "art_test1234"
	}
	return ToolArtifactRef{ID: id, Bytes: len(content)}, nil
}

// TestToolResultSpoolsArtifactWhenTruncated: an over-budget output with a sink
// present gets an artifact reference in the truncation note instead of the
// "re-run a narrower call" advice.
func TestToolResultSpoolsArtifactWhenTruncated(t *testing.T) {
	sink := &fakeArtifactSink{}
	ctx := WithToolArtifactSink(context.Background(), sink)
	raw := strings.Repeat("x", toolResultModelBytes+5000)

	env := packageToolResultCtx(ctx, "terminal", raw)
	if !env.Truncated {
		t.Fatal("expected truncation")
	}
	if len(sink.saved) != 1 || sink.saved[0] != "terminal" {
		t.Fatalf("expected one spooled output, got %v", sink.saved)
	}
	if sink.lastLen != len(raw) {
		t.Fatalf("sink must receive the full capture-capped output: %d != %d", sink.lastLen, len(raw))
	}
	if !strings.Contains(env.ModelContent, toolArtifactNoteToken+"art_test1234") {
		t.Fatalf("model content must reference the artifact: %q", env.ModelContent[len(env.ModelContent)/2-200:len(env.ModelContent)/2+200])
	}
	if !strings.Contains(env.ModelContent, "tool_output_view") {
		t.Fatal("model content must name the read-back tool")
	}
	if len(env.ModelContent) > toolResultModelBytes+1024 {
		t.Fatalf("model content must stay bounded: %d", len(env.ModelContent))
	}
}

// TestToolResultDegradesWithoutSink: no sink (or a failing sink) keeps the
// legacy head/tail note — spooling never fails a tool call.
func TestToolResultDegradesWithoutSink(t *testing.T) {
	raw := strings.Repeat("y", toolResultModelBytes+100)
	env := packageToolResultCtx(context.Background(), "terminal", raw)
	if !env.Truncated || strings.Contains(env.ModelContent, toolArtifactNoteToken) {
		t.Fatalf("no sink must degrade to the plain note: %v", env.Truncated)
	}

	sink := &fakeArtifactSink{fail: true}
	env = packageToolResultCtx(WithToolArtifactSink(context.Background(), sink), "terminal", raw)
	if !env.Truncated || strings.Contains(env.ModelContent, toolArtifactNoteToken) {
		t.Fatal("failing sink must degrade to the plain note")
	}
}

// TestToolResultSmallOutputNotSpooled: under-budget output is passed through
// verbatim and never spooled.
func TestToolResultSmallOutputNotSpooled(t *testing.T) {
	sink := &fakeArtifactSink{}
	env := packageToolResultCtx(WithToolArtifactSink(context.Background(), sink), "read_file", "small output")
	if env.Truncated || env.ModelContent != "small output" || len(sink.saved) != 0 {
		t.Fatalf("small output must pass through: %+v saved=%v", env, sink.saved)
	}
}

// TestToolResultCaptureCap: output beyond the 2MiB capture cap is head/tail
// bounded at intake, and the sink receives the capped version.
func TestToolResultCaptureCap(t *testing.T) {
	sink := &fakeArtifactSink{}
	ctx := WithToolArtifactSink(context.Background(), sink)
	raw := strings.Repeat("z", toolResultRawCapBytes+4096)

	env := packageToolResultCtx(ctx, "terminal", raw)
	if len(env.Raw) > toolResultRawCapBytes {
		t.Fatalf("raw must be capture-capped: %d > %d", len(env.Raw), toolResultRawCapBytes)
	}
	if !strings.Contains(env.Raw, "capture limit") {
		t.Fatal("capture cap must be announced in the raw surface")
	}
	if sink.lastLen > toolResultRawCapBytes {
		t.Fatalf("sink must never receive more than the capture cap: %d", sink.lastLen)
	}
}

// TestShrinkAgedToolResult: only artifact-backed content shrinks, and the
// shrunk form keeps the artifact id readable.
func TestShrinkAgedToolResult(t *testing.T) {
	head := strings.Repeat("a", 12000)
	tail := strings.Repeat("b", 12000)
	withRef := head + "[SelfMind note: ... " + toolArtifactNoteToken + "art_abcd1234 ...]" + tail
	shrunk, ok := shrinkAgedToolResult(withRef)
	if !ok {
		t.Fatal("artifact-backed content must shrink")
	}
	if len(shrunk) > toolResultAgedBytes+512 {
		t.Fatalf("shrunk content too large: %d", len(shrunk))
	}
	if !strings.Contains(shrunk, "art_abcd1234") {
		t.Fatal("shrunk content must keep the artifact id addressable")
	}

	withoutRef := head + tail
	if _, ok := shrinkAgedToolResult(withoutRef); ok {
		t.Fatal("content without an artifact reference must never shrink (lossy)")
	}
	small := "tiny " + toolArtifactNoteToken + "art_abcd1234"
	if _, ok := shrinkAgedToolResult(small); ok {
		t.Fatal("already-small content must not shrink")
	}
}
