package kernel

import (
	"context"
	"testing"

	"selfmind/internal/kernel/llm"
)

func TestBeginFlightRecording(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SELFMIND_FLIGHT_RECORDER", "1")
	t.Setenv("SELFMIND_FLIGHT_DIR", dir)
	a := &Agent{}

	// Internal/background channels are not recorded.
	for _, ch := range []string{"", "system", "delegation", "cli:background_review"} {
		ctx := context.Background()
		if fin := a.beginFlightRecording(&ctx, "t", ch, "p"); fin != nil {
			t.Fatalf("channel %q should not be recorded", ch)
		}
	}

	// A user-facing turn is recorded: session set + meta written on finalize.
	ctx := context.Background()
	fin := a.beginFlightRecording(&ctx, "default", "cli", "继续写登录")
	if fin == nil {
		t.Fatal("cli turn should be recorded")
	}
	if llm.VCRSessionForTest(ctx) == "" {
		t.Fatal("expected a VCR session to be set on the turn context")
	}
	fin("done", nil)
	if id := llm.LatestFlightID(); id == "" {
		t.Fatal("expected a latest flight id after finalize")
	}
}
