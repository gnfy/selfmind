package llm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordingProvenanceDoesNotChangeReplayOrStorePrompt(t *testing.T) {
	ctx := WithModelContext(context.Background(), ModelContext{Role: RoleCodingAgent})
	v := &vcrProvider{mode: "record"}
	path := filepath.Join(t.TempDir(), "0000.json")
	req := ChatRequest{Model: "test-model", Messages: []Message{{Role: "user", Content: "secret-source-text"}},
		Tools: []ToolDefinition{{Name: "read_file", Description: "private-tool-description"}}}
	v.saveRequest(ctx, path, cassette{Method: "completion", Completion: "ok"}, req)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var recorded cassette
	if err := json.Unmarshal(data, &recorded); err != nil {
		t.Fatal(err)
	}
	p := recorded.RecordedWith
	if p == nil || p.Model != "test-model" || p.RecordedAt == "" || p.Role != "coding_agent" || len(p.SchemaHash) != 64 {
		t.Fatalf("missing provenance: %+v", p)
	}
	for _, secret := range []string{"secret-source-text", "private-tool-description"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("raw request leaked into provenance: %s", secret)
		}
	}
	loaded, err := v.load(ctx, path, nil)
	if err != nil || loaded.Completion != "ok" {
		t.Fatalf("replay changed: %+v %v", loaded, err)
	}
	legacy := filepath.Join(t.TempDir(), "0000.json")
	if err := os.WriteFile(legacy, []byte(`{"method":"completion","completion":"old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	old, err := v.load(ctx, legacy, nil)
	if err != nil || old.RecordedWith != nil || old.Completion != "old" {
		t.Fatalf("legacy provenance invented: %+v %v", old, err)
	}
}
