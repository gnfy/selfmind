package httpapi

import (
	"context"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
)

func TestOrdinaryRootRequestCreatesSearchableUnlistedInteraction(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)
	identity, err := store.ResolveOrCreateAccount(ctx, control.DefaultTenantID, "cli", "owner", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Control: store, DefaultTenantID: control.DefaultTenantID}
	task, _, err := server.coordinator().createRootTask(ctx, identity, api.MessageRequest{
		Channel: "cli", Content: "你好，你是什么模型？",
	})
	if err != nil {
		t.Fatal(err)
	}
	timeline := control.NewWorkTimeline(store)
	listed, err := timeline.List(ctx, identity.TenantID, identity.PersonID, control.ThreadQuery{View: control.ThreadViewListed})
	if err != nil {
		t.Fatal(err)
	}
	if listed.Total != 0 {
		t.Fatalf("ordinary root request appeared in /tasks work list: %+v", listed)
	}
	search, err := timeline.Search(ctx, identity.TenantID, identity.PersonID, "什么模型", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(search) != 1 || search[0].ID != task.ID || search[0].Kind != control.ThreadKindInteraction {
		t.Fatalf("search=%+v, want interaction %s", search, task.ID)
	}
}
