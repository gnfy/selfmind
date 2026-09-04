package cliapp

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
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
	store.Close()
	// Seed a review-only invalid parent edge below the typed writer boundary.
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE runs SET resumes_run_id = 'run_missing' WHERE tenant_id = ? AND id = ?`,
		identity.TenantID, run.ID); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

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
	if out := runCommand(); !strings.Contains(out, "REVIEW") ||
		!strings.Contains(out, "illegal_parent_edge") {
		t.Fatalf("dry-run output:\n%s", out)
	}
	store, err = control.OpenStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	afterDryRun, err := store.GetRun(ctx, identity.TenantID, run.ID)
	store.Close()
	if err != nil || afterDryRun == nil || afterDryRun.ResumesRunID != "run_missing" {
		t.Fatalf("dry run mutated the edge: %+v err=%v", afterDryRun, err)
	}
	if out := runCommand("--apply"); !strings.Contains(out, "No automatic repairs were applied") {
		t.Fatalf("apply output:\n%s", out)
	}
	store, err = control.OpenStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	repaired, err := store.GetRun(ctx, identity.TenantID, run.ID)
	store.Close()
	if err != nil || repaired == nil || repaired.ResumesRunID != "run_missing" {
		t.Fatalf("review-only audit mutated the edge: %+v err=%v", repaired, err)
	}
}
