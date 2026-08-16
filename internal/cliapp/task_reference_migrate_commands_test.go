package cliapp

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
)

func TestMaintenanceMigrateTaskReferencesIsDryRunByDefault(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := control.OpenStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := store.ResolveOrCreateAccount(ctx, control.DefaultTenantID, "cli", "reference-migrate", "Reference Migration")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "legacy release", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRunWithWorkKey(ctx, task, "cli", "continue RELEASE-500", "RELEASE-500")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "done"); err != nil {
		t.Fatal(err)
	}
	store.Close()

	runCommand := func(extra ...string) string {
		out := &bytes.Buffer{}
		args := []string{"selfmind", "maintenance", "migrate-task-references", "--data-dir", dataDir}
		args = append(args, extra...)
		app := &App{ctx: ctx, args: args, stdout: out, stderr: out}
		if handled, code := app.runMaintenanceCommandIfRequested(); !handled || code != 0 {
			t.Fatalf("handled=%v code=%d output=%s", handled, code, out.String())
		}
		return out.String()
	}
	if out := runCommand(); !strings.Contains(out, "IMPORT") || !strings.Contains(out, "Re-run with --apply") {
		t.Fatalf("dry-run output:\n%s", out)
	}
	if out := runCommand("--apply"); !strings.Contains(out, "applied 1") {
		t.Fatalf("apply output:\n%s", out)
	}
	if out := runCommand("--apply"); !strings.Contains(out, "already imported 1") || !strings.Contains(out, "applied 0") {
		t.Fatalf("idempotent output:\n%s", out)
	}
}
