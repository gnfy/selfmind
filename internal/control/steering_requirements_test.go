package control

import (
	"context"
	"testing"
)

func TestRunSteeringRequirementsFollowExactLineageAndKeepLatest(t *testing.T) {
	ctx := context.Background()
	store, identity, task, parent := newRecoveryFixture(t)
	for _, text := range []string{"old detail", "use the revised title"} {
		if _, err := store.AcceptSteering(ctx, SteeringMessage{TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: parent.ID, Content: text}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.FinishRun(ctx, identity.TenantID, parent.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	child, err := store.StartRunWithOptions(ctx, task, "cli", "continue", StartRunOptions{ResumesRunID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcceptSteering(ctx, SteeringMessage{TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: child.ID, Content: "keep it a draft"}); err != nil {
		t.Fatal(err)
	}
	unrelated, err := store.StartRun(ctx, task, "cli", "other work")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcceptSteering(ctx, SteeringMessage{TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: unrelated.ID, Content: "unrelated"}); err != nil {
		t.Fatal(err)
	}
	got, err := store.RunSteeringRequirements(ctx, identity.TenantID, child.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Content != "use the revised title" || got[1].Content != "keep it a draft" {
		t.Fatalf("requirements: %+v", got)
	}
	got, err = store.RunSteeringRequirements(ctx, "other-tenant", child.ID, 10)
	if err != nil || len(got) != 0 {
		t.Fatalf("tenant isolation: %+v %v", got, err)
	}
}
