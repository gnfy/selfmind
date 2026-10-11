package llm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEffectObservationRebindsOnlyRequestClaims(t *testing.T) {
	oldID := "effectclaim_11111111-1111-4111-8111-111111111111"
	newID := "effectclaim_22222222-2222-4222-8222-222222222222"
	unknown := "effectclaim_33333333-3333-4333-8333-333333333333"
	v := &vcrProvider{}
	path := filepath.Join(t.TempDir(), "0000.json")
	request := func(id string) []Message { return []Message{{Role: "system", Content: "claim_id=" + id}} }
	c := cassette{Method: "stream", Events: []recordedEvent{{ToolCalls: []ToolCall{
		{ID: "observe", Function: "verify", Args: `{"effect_claim_id":"` + oldID + `"}`},
		{ID: "unknown", Function: "verify", Args: `{"effect_claim_id":"` + unknown + `"}`},
	}}}}
	v.save(context.Background(), path, c, request(oldID))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), oldID) {
		t.Fatal("recording retained a runtime claim ID")
	}
	got, err := v.load(context.Background(), path, request(newID))
	if err != nil {
		t.Fatal(err)
	}
	if got.Events[0].ToolCalls[0].Args != `{"effect_claim_id":"`+newID+`"}` || !strings.Contains(got.Events[0].ToolCalls[1].Args, unknown) {
		t.Fatalf("claim rebinding invented a reference: %+v", got.Events)
	}
}

func TestCassettesCarryNoVolatileEffectClaims(t *testing.T) {
	for _, file := range vcrCorpusFiles(t) {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if id := vcrEffectClaimIDPattern.Find(raw); len(id) > 0 {
			t.Errorf("%s retains a recording-time effect claim %q; re-record with request-bound identities", file, id)
		}
	}
}
