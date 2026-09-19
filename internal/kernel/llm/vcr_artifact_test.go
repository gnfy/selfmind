package llm

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestArtifactReferencesRebindAcrossReplay(t *testing.T) {
	oldID := "art_11111111-1111-1111-1111-111111111111"
	newID := "art_22222222-2222-2222-2222-222222222222"
	v := &vcrProvider{}
	path := filepath.Join(t.TempDir(), "0000.json")
	c := cassette{Method: "stream", Events: []recordedEvent{{ToolCalls: []ToolCall{{ID: "read", Function: "tool_output_view", Args: `{"artifact_id":"` + oldID + `"}`}}}}}
	v.save(context.Background(), path, c, []Message{{Role: "tool", Content: "saved as artifact " + oldID}})
	got, err := v.load(context.Background(), path, []Message{{Role: "tool", Content: "saved as artifact " + newID}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Events[0].ToolCalls[0].Args != `{"artifact_id":"`+newID+`"}` {
		t.Fatalf("recording artifact leaked into replay: %s", got.Events[0].ToolCalls[0].Args)
	}
}

func TestCassettesCarryNoVolatileArtifactIDs(t *testing.T) {
	for _, file := range vcrCorpusFiles(t) {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if id := vcrArtifactIDPattern.Find(raw); len(id) > 0 {
			t.Errorf("%s contains a recording artifact %q; re-record with request-bound artifact references", file, id)
		}
	}
}
