package kernel

import (
	"strings"
	"testing"
)

// A request recounts the same history several times, and a long run without
// spaces is the encoder's slowest input, so a long text is encoded once. The
// cached count equals a fresh encoding, texts of equal length stay distinct,
// and the cache stays within its bound.
func TestTokenEstimatorCountsALongTextOnce(t *testing.T) {
	te := NewTokenEstimator()
	if te.enc == nil {
		t.Skip("cl100k codec unavailable")
	}
	blob := strings.Repeat("QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo", 240)
	_, ids, _ := te.enc.Encode(blob)
	for i := 0; i < 3; i++ {
		if got := te.Count(blob); got != len(ids) {
			t.Fatalf("count %d = %d, want %d", i, got, len(ids))
		}
	}
	if len(te.counted) != 1 {
		t.Fatalf("cache entries=%d, want the one long text", len(te.counted))
	}
	if te.Count("short text") == 0 || len(te.counted) != 1 {
		t.Fatal("a short text entered the cache")
	}
	prose := strings.Repeat("ordinary words ", len(blob)/len("ordinary words "))
	prose += strings.Repeat(" ", len(blob)-len(prose))
	_, proseIDs, _ := te.enc.Encode(prose)
	if len(prose) != len(blob) || te.Count(prose) != len(proseIDs) || len(proseIDs) == len(ids) {
		t.Fatalf("a text of the same length reused another text's count: %d, want %d", te.Count(prose), len(proseIDs))
	}

	// A full cache starts over instead of growing past its bound.
	for i := len(te.counted); i < tokenCountCacheEntries; i++ {
		te.counted[tokenCountKey{hash: uint64(i), length: -1}] = 1
	}
	next := strings.Repeat("ordinary tool output line\n", 40)
	te.Count(next)
	if len(te.counted) != 1 {
		t.Fatalf("full cache entries=%d, want it restarted with the new text", len(te.counted))
	}
}
