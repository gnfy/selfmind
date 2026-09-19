package httpapi

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
)

func TestAttentionExplainsExactRunOutcomes(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	ctx := context.Background()
	_, older, newer := seedTwoAttentionRunsInOneThread(t, store, identity, "same history")
	for _, record := range []struct {
		run           *control.Run
		summary, next string
	}{
		{older, "Release is waiting for consent", "Confirm the release"},
		{newer, "Required evidence is incomplete", "Recheck the changed input"},
	} {
		if _, err := store.AppendEvent(ctx, control.Event{RunID: record.run.ID, Type: "run.finished", Payload: mustJSON(map[string]interface{}{"outcome": map[string]interface{}{"summary": record.summary, "next_steps": []string{record.next}, "verification": map[string]interface{}{"summary": "Input changed after verification"}}})}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.FinishRun(ctx, identity.TenantID, newer.ID, "verification_partial"); err != nil {
		t.Fatal(err)
	}
	text := controlReply(t, daemon, "/resume")
	for _, want := range []string{"Release is waiting for consent", "Confirm the release", "Required evidence is incomplete", "Verification: Input changed after verification", "verification incomplete"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, "Recheck the changed input") {
		t.Fatal("optional model next step displaced runtime verification gap")
	}
	foreign, err := store.RunAttentionOutcomes(ctx, identity.TenantID, "another-person", []string{older.ID, newer.ID})
	if err != nil || len(foreign) != 0 {
		t.Fatalf("foreign outcomes=%+v err=%v", foreign, err)
	}
	if got := attentionActivityLabel(control.AttentionItem{Activity: "resumable", RunStatus: "waiting_user"}, nil); got != "waiting for your decision" {
		t.Fatal(got)
	}
}
