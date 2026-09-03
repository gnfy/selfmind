package cliapp

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/kernel/memory"
)

func TestMaintenanceResetWorkHistoryIsDryRunAndApplyCreatesBackup(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := control.OpenStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.ResolveOrCreateAccount(ctx, control.DefaultTenantID, "cli", "reset-test", "Reset Test")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: id.TenantID, PersonID: id.PersonID, Title: "old work"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "old work")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, id.TenantID, run.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// Memory lives beside control.db: a Thread-keyed session and a preference
	// that cites the Run must lose their work-history references, while the
	// plain session and the preference itself survive.
	provider, err := memory.NewSQLiteProvider(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.IndexSession(id.PersonID, memory.FTS5Session{SessionID: "task:" + task.ID, Channel: "cli", Content: "old work transcript", Timestamp: 1}); err != nil {
		t.Fatal(err)
	}
	if err := provider.IndexSession(id.PersonID, memory.FTS5Session{SessionID: "session", Channel: "cli", Content: "plain conversation", Timestamp: 2}); err != nil {
		t.Fatal(err)
	}
	if err := provider.AddFactMeta(ctx, id.PersonID, memory.Fact{Target: "user", Content: "prefers concise replies", Source: "user", Scope: "global", CreatedFromRun: run.ID}); err != nil {
		t.Fatal(err)
	}
	if err := provider.Close(); err != nil {
		t.Fatal(err)
	}

	runCommand := func(extra ...string) (int, string) {
		out := &bytes.Buffer{}
		args := []string{"selfmind", "maintenance", "reset-work-history", "--data-dir", dataDir}
		args = append(args, extra...)
		app := &App{ctx: ctx, args: args, stdout: out, stderr: out}
		handled, code := app.runMaintenanceCommandIfRequested()
		if !handled {
			t.Fatal("maintenance command was not handled")
		}
		return code, out.String()
	}
	if code, out := runCommand(); code != 0 || !strings.Contains(out, "Dry run only") || !strings.Contains(out, "1 thread(s), 1 run(s)") ||
		!strings.Contains(out, "In-flight Skill learning evidence") || !strings.Contains(out, "published Skill packages would be preserved") {
		t.Fatalf("dry run code=%d output:\n%s", code, out)
	}
	store, err = control.OpenStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if preview, err := store.PreviewWorkHistoryReset(ctx, id.TenantID); err != nil || preview.Threads != 1 {
		t.Fatalf("dry run mutated history: %+v err=%v", preview, err)
	}
	_ = store.Close()

	code, out := runCommand("--apply")
	if code != 0 || !strings.Contains(out, "Removed 1 thread(s), 1 run(s)") || !strings.Contains(out, "Identity") {
		t.Fatalf("apply code=%d output:\n%s", code, out)
	}
	if !strings.Contains(out, "Memory: removed 1 task session(s) and cleared ") || !strings.Contains(out, "in 1 partition(s)") ||
		!strings.Contains(out, "in-flight Skill learning evidence was removed") {
		t.Fatalf("apply output lacks the memory and Skill-evidence summary:\n%s", out)
	}
	matches, err := filepath.Glob(filepath.Join(dataDir, "backups", "control-before-work-history-reset-*.db"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("backup matches=%v err=%v", matches, err)
	}
	if info, err := os.Stat(matches[0]); err != nil || info.Size() == 0 {
		t.Fatalf("backup info=%v err=%v", info, err)
	}
	store, err = control.OpenStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if preview, err := store.PreviewWorkHistoryReset(ctx, id.TenantID); err != nil || preview.Threads != 0 || preview.Runs != 0 {
		t.Fatalf("history remains: %+v err=%v", preview, err)
	}
	if _, err := store.CurrentWorkspace(ctx, id.TenantID, id.PersonID); err != nil {
		t.Fatalf("owner configuration was not preserved: %v", err)
	}
	provider, err = memory.NewSQLiteProvider(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	if sessions, err := provider.ListRecentSessions(id.PersonID, 5); err != nil || len(sessions) != 1 || sessions[0].SessionID != "session" {
		t.Fatalf("memory sessions after reset=%+v err=%v, want only the plain session", sessions, err)
	}
	facts, err := provider.GetFacts(ctx, id.PersonID, "user")
	if err != nil || len(facts) != 1 || facts[0].Content != "prefers concise replies" || facts[0].CreatedFromRun != "" {
		t.Fatalf("preference facts after reset=%+v err=%v, want the preference without its run reference", facts, err)
	}
}

func TestMaintenanceResetWorkHistoryRefusesRunningRunBeforeBackup(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := control.OpenStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.ResolveOrCreateAccount(ctx, control.DefaultTenantID, "cli", "reset-live", "Reset Live")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: id.TenantID, PersonID: id.PersonID, Title: "live work"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartRun(ctx, task, "cli", "live work"); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	out := &bytes.Buffer{}
	app := &App{ctx: ctx, args: []string{"selfmind", "maintenance", "reset-work-history", "--data-dir", dataDir, "--apply"}, stdout: out, stderr: out}
	if handled, code := app.runMaintenanceCommandIfRequested(); !handled || code != 1 || !strings.Contains(out.String(), "refusing while live work exists") {
		t.Fatalf("handled=%v code=%d output=%s", handled, code, out.String())
	}
	matches, err := filepath.Glob(filepath.Join(dataDir, "backups", "control-before-work-history-reset-*.db"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("refusal created backup matches=%v err=%v", matches, err)
	}
}
