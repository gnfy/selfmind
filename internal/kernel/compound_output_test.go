package kernel

import (
	"context"
	"strings"
	"testing"
)

func TestCompoundToolBudgetUsesSharedCaptureAndArtifactPath(t *testing.T) {
	sink := &fakeArtifactSink{}
	raw := strings.Repeat("prefix", 1000) + "middle-fact" + strings.Repeat("suffix", 1000)
	env := BoundToolOutput(WithToolArtifactSink(context.Background(), sink), "read_file", raw, 8192)
	if !env.Truncated || env.ArtifactID == "" || sink.lastLen != len(raw) || len(env.ModelContent) > 8192 {
		t.Fatalf("compound budget lost raw capture or exceeded output bound: bytes=%d saved=%d", len(env.ModelContent), sink.lastLen)
	}
	env = BoundToolOutput(WithToolArtifactSink(context.Background(), sink), "read_file", strings.Repeat("x", toolResultRawCapBytes+100), 8192)
	if !env.CaptureTruncated || !strings.Contains(env.ModelContent, "cannot be recovered") || sink.lastLen > toolResultRawCapBytes {
		t.Fatal("capture loss looked fully recoverable")
	}
	env = BoundToolOutput(context.Background(), "read_file", strings.Repeat("x", toolResultRawCapBytes+100), 8192)
	if len(env.ModelContent) > 8192 || !env.CaptureTruncated {
		t.Fatal("capture warning exceeded the compound model budget")
	}
}
