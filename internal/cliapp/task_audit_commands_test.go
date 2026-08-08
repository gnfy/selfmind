package cliapp

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
)

func TestMaintenanceTaskAuditIsDryRunByDefault(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := control.OpenStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := store.ResolveOrCreateAccount(ctx, control.DefaultTenantID, "cli", "task-audit", "Task Audit")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "waiting for operator", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "waiting for operator")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTaskStatus(ctx, identity.TenantID, task.ID, "waiting_user", "waiting", nil); err != nil {
		t.Fatal(err)
	}
	store.Close()

	runCommand := func(extra ...string) string {
		out := &bytes.Buffer{}
		args := []string{"selfmind", "maintenance", "task-audit", "--data-dir", dataDir}
		args = append(args, extra...)
		app := &App{ctx: ctx, args: args, stdout: out, stderr: out}
		if handled, code := app.runMaintenanceCommandIfRequested(); !handled || code != 0 {
			t.Fatalf("handled=%v code=%d output=%s", handled, code, out.String())
		}
		return out.String()
	}
	if out := runCommand(); !strings.Contains(out, "BACKFILL") || !strings.Contains(out, "Re-run with --apply") {
		t.Fatalf("dry-run output:\n%s", out)
	}
	store, err = control.OpenStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	blockers, err := store.ListOpenTaskBlockers(ctx, identity.TenantID, task.ID, 10)
	store.Close()
	if err != nil || len(blockers) != 0 {
		t.Fatalf("dry run mutated blockers: %+v err=%v", blockers, err)
	}
	if out := runCommand("--apply"); !strings.Contains(out, "applied 1") {
		t.Fatalf("apply output:\n%s", out)
	}
}
