package httpapi

import (
	"context"
	"selfmind/internal/gateway/router"
	"testing"
)

func TestNaturalLanguageIntentIsMainOwned(t *testing.T) {
	server := &Server{}
	for _, input := range []string{"继续", "开始执行", "go ahead", "sigamos", "進めてください", "按你刚才的方案办", "a new question"} {
		got := server.classifyIntent(context.Background(), input, "cli")
		if got.Intent != router.IntentTask || got.Source != "main" {
			t.Fatalf("%q was classified before Main: %+v", input, got)
		}
	}
}
