package control

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResetWorkHistoryBacksUpAndPreservesOwnerConfiguration(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := OpenStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, DefaultTenantID, "cli", "local", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := store.RegisterWorkspace(ctx, Workspace{
		TenantID: identity.TenantID, OwnerPersonID: identity.PersonID,
		Name: "project", LocalPath: filepath.Join(dataDir, "project"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetPersonSetting(ctx, identity.TenantID, identity.PersonID, "test.preference", "kept"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO skill_versions(
		control_tenant_id, skill_key, skill_name, version_hash, state, created_by, created_at
	) VALUES(?, 'skill:test', 'test', 'v1', 'active', 'user', 1)`, identity.TenantID); err != nil {
		t.Fatal(err)
	}

	timeline := NewWorkTimeline(store)
	thread, err := timeline.CreateInteraction(ctx, ThreadCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		WorkspaceID: workspace.ID, Title: "historical work", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, thread.legacyTask(), "cli", "historical work")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(ctx, Event{TaskID: thread.ID, RunID: run.ID, Type: "run.finished", Visibility: "task"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnqueueQueued(ctx, QueuedTask{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Channel: "cli", Content: "queued work",
	}); err != nil {
		t.Fatal(err)
	}

	preview, err := store.PreviewWorkHistoryReset(ctx, identity.TenantID)
	if err != nil || preview.Threads != 1 || preview.Runs != 1 || preview.QueueRows != 1 || preview.HasLiveWork() {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	backup, err := store.BackupWorkHistorySnapshot(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(backup); err != nil || info.Size() == 0 {
		t.Fatalf("backup=%q info=%v err=%v", backup, info, err)
	}
	removed, err := store.ResetWorkHistory(ctx, identity.TenantID)
	if err != nil || removed.Threads != 1 || removed.Runs != 1 || removed.QueueRows != 1 {
		t.Fatalf("removed=%+v err=%v", removed, err)
	}
	if after, err := store.PreviewWorkHistoryReset(ctx, identity.TenantID); err != nil || after != (WorkHistoryResetPreview{}) {
		t.Fatalf("after=%+v err=%v", after, err)
	}

	for table, query := range map[string]string{
		"persons":         `SELECT COUNT(*) FROM persons WHERE tenant_id = 'default'`,
		"accounts":        `SELECT COUNT(*) FROM accounts WHERE tenant_id = 'default'`,
		"workspaces":      `SELECT COUNT(*) FROM workspaces WHERE tenant_id = 'default'`,
		"person_settings": `SELECT COUNT(*) FROM person_settings WHERE tenant_id = 'default'`,
		"skill_versions":  `SELECT COUNT(*) FROM skill_versions WHERE control_tenant_id = 'default'`,
	} {
		var count int
		if err := store.db.QueryRowContext(ctx, query).Scan(&count); err != nil || count != 1 {
			t.Fatalf("preserved %s count=%d err=%v", table, count, err)
		}
	}
	backupDB, err := sql.Open("sqlite", backup)
	if err != nil {
		t.Fatal(err)
	}
	defer backupDB.Close()
	if err := quickCheckDB(ctx, backupDB); err != nil {
		t.Fatalf("backup cannot be reopened: %v", err)
	}
}

func TestResetWorkHistoryRefusesEachLiveWorkClass(t *testing.T) {
	tests := []struct {
		name string
		seed func(context.Context, *Store, *IdentityContext, *Task, *Run) error
	}{
		{name: "running run"},
		{name: "live watcher", seed: func(ctx context.Context, store *Store, id *IdentityContext, task *Task, run *Run) error {
			if err := store.FinishRun(ctx, id.TenantID, run.ID, "waiting_external"); err != nil {
				return err
			}
			_, err := store.db.ExecContext(ctx, `INSERT INTO external_watches(
				id, tenant_id, person_id, thread_id, run_id, cwd, command, success_pattern,
				status, timeout_at, next_check_at, created_at, updated_at
			) VALUES('watch-live', ?, ?, ?, ?, '/tmp', 'true', 'ok', 'pending', 9999999999, 1, 1, 1)`,
				id.TenantID, id.PersonID, task.ID, run.ID)
			return err
		}},
		{name: "started queue", seed: func(ctx context.Context, store *Store, id *IdentityContext, task *Task, run *Run) error {
			if err := store.FinishRun(ctx, id.TenantID, run.ID, "done"); err != nil {
				return err
			}
			_, err := store.db.ExecContext(ctx, `INSERT INTO task_queue(
				id, tenant_id, person_id, channel, platform, content, status, created_at
			) VALUES('queue-live', ?, ?, 'cli', 'cli', 'work', 'started', 1)`, id.TenantID, id.PersonID)
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := OpenStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			id, err := store.ResolveOrCreateAccount(ctx, DefaultTenantID, "cli", "local", "Owner")
			if err != nil {
				t.Fatal(err)
			}
			task, err := store.CreateTask(ctx, TaskCreate{TenantID: id.TenantID, PersonID: id.PersonID, Title: tt.name})
			if err != nil {
				t.Fatal(err)
			}
			run, err := store.StartRun(ctx, task, "cli", tt.name)
			if err != nil {
				t.Fatal(err)
			}
			if tt.seed != nil {
				if err := tt.seed(ctx, store, id, task, run); err != nil {
					t.Fatal(err)
				}
			}
			before, err := store.PreviewWorkHistoryReset(ctx, id.TenantID)
			if err != nil || !before.HasLiveWork() {
				t.Fatalf("preview=%+v err=%v", before, err)
			}
			if _, err := store.ResetWorkHistory(ctx, id.TenantID); !errors.Is(err, ErrLiveWorkPreventsReset) {
				t.Fatalf("reset error=%v", err)
			}
			if after, _ := store.PreviewWorkHistoryReset(ctx, id.TenantID); after.Threads != 1 || after.Runs != 1 {
				t.Fatalf("refused reset changed history: %+v", after)
			}
		})
	}
}

func TestWorkHistoryResetBackupCanBeRestored(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := OpenStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.ResolveOrCreateAccount(ctx, DefaultTenantID, "cli", "restore-reset", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{TenantID: id.TenantID, PersonID: id.PersonID, Title: "restore me"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "restore me")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, id.TenantID, run.ID, "done"); err != nil {
		t.Fatal(err)
	}
	backup, err := store.BackupWorkHistorySnapshot(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResetWorkHistory(ctx, id.TenantID); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreControlDatabase(ctx, dataDir, backup); err != nil {
		t.Fatalf("restore reset backup: %v", err)
	}
	restored, err := OpenStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	preview, err := restored.PreviewWorkHistoryReset(ctx, id.TenantID)
	if err != nil || preview.Threads != 1 || preview.Runs != 1 {
		t.Fatalf("restored preview=%+v err=%v", preview, err)
	}
}

func TestResetWorkHistoryDetachesSkillLearningEvidenceButKeepsPackages(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	id, err := store.ResolveOrCreateAccount(ctx, DefaultTenantID, "cli", "skill-evidence", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{TenantID: id.TenantID, PersonID: id.PersonID, Title: "learned work"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "learned work")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, id.TenantID, run.ID, "done"); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.db.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	// A published package cites the observations it was learned from; a guard
	// names the Run that recorded the failure; candidate snapshots cite
	// observations that the reset removes, that live elsewhere, or nothing.
	exec(`INSERT INTO skill_versions(control_tenant_id, skill_key, skill_name, version_hash, state, created_by, created_at, source_observation_ids_json)
		VALUES(?, 'skill:learned', 'learned', 'v1', 'active', 'curator', 1, '["obs-1"]')`, id.TenantID)
	exec(`INSERT INTO skill_failure_guards(control_tenant_id, skill_key, version_hash, failure_signature, source_run_id, created_at)
		VALUES(?, 'skill:learned', 'v1', 'sig-reset', ?, 1), (?, 'skill:learned', 'v1', 'sig-other', 'run_elsewhere', 1)`, id.TenantID, run.ID, id.TenantID)
	exec(`INSERT INTO workflow_observations(id, identity_tenant_id, control_tenant_id, person_id, run_id, work_unit_id, workflow_signature, outcome_status, created_at)
		VALUES('obs-1', ?, ?, ?, ?, 'wu-1', 'sig', 'done', 1), ('obs-2', ?, ?, ?, ?, 'wu-2', 'sig', 'done', 2)`,
		id.TenantID, id.TenantID, id.PersonID, run.ID, id.TenantID, id.TenantID, id.PersonID, run.ID)
	exec(`INSERT INTO skill_candidate_evidence_snapshots(control_tenant_id, skill_key, version_hash, evidence_set_hash, observation_ids_json, created_at) VALUES
		(?, 'skill:learned', 'v1', 'set-removed', '["obs-1","obs-2"]', 1),
		(?, 'skill:learned', 'v2', 'set-foreign', '["obs-foreign"]', 1),
		(?, 'skill:learned', 'v3', 'set-empty', '[]', 1),
		(?, 'skill:learned', 'v4', 'set-mixed', '["obs-1","obs-foreign"]', 1)`, id.TenantID, id.TenantID, id.TenantID, id.TenantID)

	if _, err := store.ResetWorkHistory(ctx, id.TenantID); err != nil {
		t.Fatal(err)
	}

	for signature, want := range map[string]string{"sig-reset": "", "sig-other": "run_elsewhere"} {
		var source string
		if err := store.db.QueryRowContext(ctx, `SELECT source_run_id FROM skill_failure_guards WHERE failure_signature = ?`, signature).Scan(&source); err != nil || source != want {
			t.Fatalf("guard %s source_run_id=%q err=%v, want %q", signature, source, err, want)
		}
	}
	// Every citation list keeps exactly the observations that still exist: a
	// mixed list loses only the removed id, and an id this reset never removed
	// (another tenant's, or one that was already gone) is left alone.
	for set, want := range map[string]string{
		"set-removed": "[]", "set-foreign": `["obs-foreign"]`, "set-empty": "[]", "set-mixed": `["obs-foreign"]`,
	} {
		var cited string
		if err := store.db.QueryRowContext(ctx, `SELECT observation_ids_json FROM skill_candidate_evidence_snapshots WHERE evidence_set_hash = ?`, set).Scan(&cited); err != nil || cited != want {
			t.Fatalf("snapshot %s observation_ids_json=%q err=%v, want %q", set, cited, err, want)
		}
	}
	var observations int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM workflow_observations`).Scan(&observations); err != nil || observations != 0 {
		t.Fatalf("workflow observations after reset=%d err=%v", observations, err)
	}
	// The published package itself survives a work-history reset, but its
	// provenance may not keep naming observations the reset deleted.
	var packages int
	var sources string
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*), MAX(source_observation_ids_json) FROM skill_versions WHERE control_tenant_id = ?`, id.TenantID).Scan(&packages, &sources); err != nil || packages != 1 || sources != `[]` {
		t.Fatalf("published package rows=%d sources=%q err=%v, want 1 package with no dangling provenance", packages, sources, err)
	}
}
