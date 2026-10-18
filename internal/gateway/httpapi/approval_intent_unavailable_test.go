package httpapi

import (
	"context"
	"testing"

	"selfmind/internal/tools"
)

func TestUnavailableSteeringCannotMasqueradeAsUnchangedIntent(t *testing.T) {
	daemon, store, identity := newTaskViewServer(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	base := tools.RunIntentSnapshot{ModelAuthorization: true, RawUserText: "Inspect and report."}
	got := daemon.coordinator().intentWithAddedRequirements(context.Background(), identity, base, "exact-run")
	if !got.AuthorizationEvidenceIncomplete || got.RawUserText != base.RawUserText {
		t.Fatalf("failed fresh read advertised an unchanged complete snapshot: %+v", got)
	}
}
