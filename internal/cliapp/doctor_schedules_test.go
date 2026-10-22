package cliapp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/kernel/task/cron"
)

// doctor shows the reviewer's schedules that do not run and the unclaimed
// ones, each with its reason and how to remove it, and only counts other
// people's.
func TestDoctorShowsSchedulesThatDoNotRun(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := controltest.NewStoreInDir(t, dir)
	t.Cleanup(func() { store.Close() })
	me, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "cwill", "Me")
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx-stranger", "Stranger")
	if err != nil {
		t.Fatal(err)
	}
	ranWork := func(person *control.IdentityContext, receipt string) {
		task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, Title: "t", Channel: "c"})
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.StartRun(ctx, task, "c", "work")
		if err != nil {
			t.Fatal(err)
		}
		if receipt != "" {
			payload, _ := json.Marshal(map[string]string{"tool": "cron", "result": receipt})
			if _, err := store.AppendEvent(ctx, control.Event{TaskID: task.ID, RunID: run.ID, Type: "tool.completed", Payload: payload}); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.FinishRun(ctx, person.TenantID, run.ID, "done"); err != nil {
			t.Fatal(err)
		}
	}
	ranWork(stranger, "")

	db, err := sql.Open("sqlite", filepath.Join(dir, "cron.db"))
	if err != nil {
		t.Fatal(err)
	}
	sched := cron.NewScheduler(db, nil)
	if err := sched.InitSchema(ctx); err != nil {
		t.Fatal(err)
	}
	add := func(name, platform, deliverTo, channel, creator string, enabled bool) int64 {
		result, err := db.Exec(`INSERT INTO cron_jobs (name, cron_expr, prompt, tenant_id, channel, platform, deliver_to, creator_person_id, enabled)
			VALUES (?, '0 9 * * *', 'p', 'default', ?, ?, ?, ?, ?)`, name, channel, platform, deliverTo, creator, enabled)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := result.LastInsertId()
		return id
	}
	group := add("team digest", "weixin", "team@chatroom", "team@chatroom", "", false)
	ranWork(me, fmt.Sprintf("Cron job %d (%s) added.", group, "team digest"))
	orphan := add("old report", "", "", "cli", "", true)
	add("their private plan", "weixin", "wx-stranger", "wx-stranger", stranger.PersonID, true)
	add("my cli job", "cli", "cwill", "cli", me.PersonID, true)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	section := scheduleReviewSection(ctx, store, me, dir)
	for _, want := range []string{
		"jobs: 2 yours, 1 unclaimed, 1 belong to other people (not shown)",
		fmt.Sprintf(`warning: job %d "team digest" (off) does not run: it names weixin:team@chatroom, which is not bound to anyone here`, group),
		fmt.Sprintf(`remove it: selfmind send "remove cron job %d"`, group),
		fmt.Sprintf(`warning: unclaimed job %d "old report" (on) does not run: it names no account`, orphan),
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("schedules section is missing %q:\n%s", want, section)
		}
	}
	if strings.Contains(section, "their private plan") || strings.Contains(section, "my cli job") {
		t.Fatalf("the section shows another person's job or a healthy one:\n%s", section)
	}
	issues := collectDoctorIssues("SelfMind doctor\n\n== Schedules ==\n"+section, configDiagnostics{Path: "/tmp/config.yaml"})
	if len(issues) != 1 || issues[0].Category != "SCHEDULE" || !strings.Contains(issues[0].Details, "team digest") {
		t.Fatalf("issues = %+v", issues)
	}

	// The variant: a reviewer whose schedules all run sees no warning.
	if healthy := scheduleReviewSection(ctx, store, stranger, dir); strings.Contains(healthy, "warning:") ||
		!strings.Contains(healthy, "jobs: 1 yours") {
		t.Fatalf("the stranger's review:\n%s", healthy)
	}
	if missing := scheduleReviewSection(ctx, store, me, t.TempDir()); missing != "(no schedules)\n" {
		t.Fatalf("no cron database: %q", missing)
	}
}
