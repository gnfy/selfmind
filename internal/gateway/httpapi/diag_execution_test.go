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
