package control

import (
	"context"
	"testing"
)

func TestRunDeliveryOverrideUsesBoundSteeringEndpointAndSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wx-bound", "Bound IM"); err != nil {
		t.Fatal(err)
	}
	task, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "release", Channel: "cli"})
	run, _ := store.StartRun(ctx, task, "cli", "release")
	steering, err := store.AcceptSteering(ctx, SteeringMessage{
		TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: run.ID, TaskID: task.ID,
		Platform: "weixin", PlatformUserID: "wx-bound", Channel: "wx-chat", Content: "send the final result here",
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.SetRunDeliveryOverrideFromSteering(ctx, identity.TenantID, identity.PersonID, run.ID, steering.ID)
	if err != nil {
		t.Fatal(err)
	}
	if target.Platform != "weixin" || target.PlatformUserID != "wx-bound" || target.Channel != "wx-chat" || target.SourceSteeringID != steering.ID {
		t.Fatalf("target = %+v", target)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, err := reopened.RunDeliveryOverride(ctx, identity.TenantID, identity.PersonID, run.ID)
	if err != nil || persisted == nil || persisted.PlatformUserID != "wx-bound" || persisted.SourceSteeringID != steering.ID {
		t.Fatalf("persisted = %+v err=%v", persisted, err)
	}
}

func TestRunDeliveryOverrideRejectsUnboundOrForeignEndpoint(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	alice, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "alice", "Alice")
	bob, _ := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx-bob", "Bob")
	task, _ := store.CreateTask(ctx, TaskCreate{TenantID: alice.TenantID, PersonID: alice.PersonID, Title: "release", Channel: "cli"})
	run, _ := store.StartRun(ctx, task, "cli", "release")
	foreign, _ := store.AcceptSteering(ctx, SteeringMessage{
		TenantID: alice.TenantID, PersonID: alice.PersonID, RunID: run.ID, TaskID: task.ID,
		Platform: "weixin", PlatformUserID: bob.PlatformUserID, Channel: "wx-chat", Content: "send here",
	})
	if _, err := store.SetRunDeliveryOverrideFromSteering(ctx, alice.TenantID, alice.PersonID, run.ID, foreign.ID); err == nil {
		t.Fatal("unbound endpoint must not receive a delivery override")
	}
	cli, _ := store.AcceptSteering(ctx, SteeringMessage{
		TenantID: alice.TenantID, PersonID: alice.PersonID, RunID: run.ID, TaskID: task.ID,
		Platform: "cli", PlatformUserID: "alice", Channel: "cli", Content: "send here",
	})
	if _, err := store.SetRunDeliveryOverrideFromSteering(ctx, alice.TenantID, alice.PersonID, run.ID, cli.ID); err == nil {
		t.Fatal("cli has no push delivery surface")
	}
}
