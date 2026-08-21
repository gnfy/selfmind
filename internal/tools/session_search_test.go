package tools

import (
	"errors"
	"strings"
	"testing"
)

func TestSessionSearchReturnsStableFailureEnvelope(t *testing.T) {
	tool := NewSessionSearchTool()
	tool.RegisterSearchFn(func(string, int) (interface{}, error) {
		return nil, errors.New("SQL logic error: no such column: 625 (1)")
	})

	_, err := tool.Execute(map[string]interface{}{"query": "625"})
	if err == nil {
		t.Fatal("expected session search failure")
	}
	if !strings.Contains(err.Error(), "no such column: 625") {
		t.Fatalf("local cause was not preserved: %v", err)
	}
	stable, ok := err.(interface {
		ToolErrorCode() string
		ToolErrorCategory() string
		ModelSafeMessage() string
		ToolRecoveryHint() string
	})
	if !ok {
		t.Fatalf("session search error does not expose stable metadata: %T", err)
	}
	if stable.ToolErrorCode() != "session_search_unavailable" || stable.ToolErrorCategory() != "data_store" {
		t.Fatalf("stable error metadata = %s/%s", stable.ToolErrorCode(), stable.ToolErrorCategory())
	}
	if strings.Contains(stable.ModelSafeMessage(), "SQL") || !strings.Contains(stable.ToolRecoveryHint(), "simpler literal text") {
		t.Fatalf("unsafe or unactionable stable error: message=%q hint=%q", stable.ModelSafeMessage(), stable.ToolRecoveryHint())
	}
}
