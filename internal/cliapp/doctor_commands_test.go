package cliapp

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appcore "selfmind/internal/app"
	"selfmind/internal/tools"

	_ "modernc.org/sqlite"
)

func TestFormatModelRoleProbesGroupsResultsAndReportsFailure(t *testing.T) {
	section, failed := formatModelRoleProbes([]appcore.ModelRoleProbe{
		{Roles: []string{"background_review", "memory_extract"}, Provider: "kimi-coding", Model: "kimi-for-coding", Latency: 1250 * time.Millisecond},
		{Roles: []string{"summarizer"}, Provider: "minimax", Model: "MiniMax-M3", Latency: time.Second, Err: errors.New("provider 403: secret-token")},
	})
	if !failed {
		t.Fatal("failed probe must make doctor fail")
	}
	for _, want := range []string{
		"OK roles=background_review,memory_extract provider=kimi-coding model=kimi-for-coding",
		"FAIL roles=summarizer provider=minimax model=MiniMax-M3",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("probe section missing %q:\n%s", want, section)
		}
	}
}

func TestFormatModelRoleProbesHandlesNoConfiguredRoles(t *testing.T) {
	section, failed := formatModelRoleProbes(nil)
	if failed || !strings.Contains(section, "no auxiliary model or explicit role overrides") {
		t.Fatalf("section=%q failed=%v", section, failed)
	}
}

func TestFormatSkillPartitionDiagnosticsIsActionable(t *testing.T) {
	healthy := formatSkillPartitionDiagnostics(tools.SkillMigrationReport{}, nil)
	if !strings.Contains(healthy, "healthy") {
		t.Fatalf("healthy section = %q", healthy)
	}
	pending := formatSkillPartitionDiagnostics(tools.SkillMigrationReport{
		Partitions: 2,
		Migrated:   3,
		Deduped:    1,
		Conflicts:  1,
	}, nil)
	for _, want := range []string{"partitions=2", "migrate-skills", "conflicts=1"} {
		if !strings.Contains(pending, want) {
			t.Fatalf("pending section missing %q: %s", want, pending)
		}
	}
}

func TestCronGovernanceIgnoresHistoricalUserReservedPrefix(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "cron.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE cron_jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT, cron_expr TEXT, prompt TEXT,
		tenant_id TEXT, channel TEXT, system_key TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO cron_jobs
		(name, cron_expr, prompt, tenant_id, channel, system_key) VALUES
		('skill-pruner-default', '0 3 * * *', 'skill_prune:default', 'default', 'cli', 'skill-pruner:default'),
		('skill-pruner-user-report', '15 9 * * 1', 'send report', 'default', 'weixin', '')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	section := cronGovernanceSection(context.Background(), dir)
	if !strings.Contains(section, "skill-pruner: 1 keyed + 0 legacy") {
		t.Fatalf("unexpected cron governance section:\n%s", section)
	}
	if strings.Contains(section, "legacy system-shaped skill-pruner") {
		t.Fatalf("historical user job must not trigger a system warning:\n%s", section)
	}
}

func TestCronGovernanceReportsLegacySystemShape(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "cron.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE cron_jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT, cron_expr TEXT, prompt TEXT,
		tenant_id TEXT, channel TEXT, system_key TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO cron_jobs
		(name, cron_expr, prompt, tenant_id, channel, system_key)
		VALUES ('skill-pruner-person_abc', '0 3 * * *', 'skill_prune:person_abc', 'person_abc', 'cli', '')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	section := cronGovernanceSection(context.Background(), dir)
	if !strings.Contains(section, "skill-pruner: 0 keyed + 1 legacy") ||
		!strings.Contains(section, "legacy system-shaped skill-pruner") {
		t.Fatalf("legacy system-shaped row was not reported:\n%s", section)
	}
}
