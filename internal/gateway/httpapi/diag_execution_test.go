package httpapi

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
)

func TestExecutionDiagIsRedactedAndShowsWorkspace(t *testing.T) {
	store := controltest.NewStore(t)
	ctx := context.Background()
	identity := &control.IdentityContext{
		TenantID: "tenant-exec-diag",
		PersonID: "person-exec-diag",
		Platform: "cli",
	}
	if _, err := store.EnsureWorkspace(ctx, control.Workspace{
		TenantID:      identity.TenantID,
		OwnerPersonID: identity.PersonID,
		Name:          "project",
		LocalPath:     "/work/project",
	}); err != nil {
		t.Fatal(err)
	}
	server := &Server{Control: store}
	handled, reply, _, err := server.tryHandleControlCommand(ctx, identity, api.MessageRequest{
		Channel: "cli:person-exec-diag",
		Content: "/diag execution",
	})
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	for _, want := range []string{
		"Execution diagnostics",
		"Sandbox backend:",
		"Process environment:",
		"Workspace: project",
		"Workspace trust: untrusted",
		"Writable root: /work/project",
		"Workspace capabilities: none",
		"Environment lease: none",
		"Credential values: hidden",
	} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply missing %q:\n%s", want, reply)
		}
	}
}

func TestExecutionDiagDoesNotCallMultipleRunsIdle(t *testing.T) {
	store := controltest.NewStore(t)
	identity := &control.IdentityContext{TenantID: "tenant-multi-diag", PersonID: "person-multi-diag", Platform: "cli"}
	server := &Server{Control: store}
	coord := server.coordinator()
	coord.activeLimit = 2
	for _, runID := range []string{"run-one", "run-two"} {
		handle := &activeRun{PersonID: identity.PersonID, RunID: runID, Channel: runID}
		if !coord.beginActive(identity.PersonID, handle) {
			t.Fatal("could not register active run")
		}
		defer coord.endActiveRun(identity.PersonID, handle)
	}
	handled, reply, _, err := server.tryHandleControlCommand(context.Background(), identity, api.MessageRequest{
		Channel: "cli:person-multi-diag", Content: "/diag execution",
	})
	if err != nil || !handled || !strings.Contains(reply, "2 active runs") || strings.Contains(reply, "no active run") {
		t.Fatalf("multi-run execution diagnostics: handled=%v err=%v reply=%q", handled, err, reply)
	}
}

func TestExecutionDiagShowsOnlyOwnersUnresolvedExternalEffects(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)
	owner := &control.IdentityContext{TenantID: "default", PersonID: "person-owner", Platform: "cli"}
	other := &control.IdentityContext{TenantID: "default", PersonID: "person-other", Platform: "cli"}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "deploy", Channel: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRunWithOptions(ctx, task, "owner", "deploy", control.StartRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimExternalEffects(ctx, control.ExternalEffectClaimRequest{
		TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: run.ID,
		EffectID: "effect-one", TargetKeys: []string{control.UnknownExternalTarget},
	}); err != nil || !claimed.Granted {
		t.Fatalf("setup claim: %+v, %v", claimed, err)
	}
	server := &Server{Control: store}
	for _, tc := range []struct {
		identity *control.IdentityContext
		want     string
	}{
		{owner, "unknown target (person-wide)"},
		{other, "External effects unresolved: none"},
	} {
		handled, reply, _, err := server.tryHandleControlCommand(ctx, tc.identity, api.MessageRequest{Content: "/diag execution"})
		if err != nil || !handled || !strings.Contains(reply, tc.want) {
			t.Fatalf("diagnostic for %s: handled=%v err=%v reply=%q", tc.identity.PersonID, handled, err, reply)
		}
		if tc.identity == other && strings.Contains(reply, "person-wide") {
			t.Fatalf("another person's claim leaked into diagnostics: %s", reply)
		}
	}
}
