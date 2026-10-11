package app

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"selfmind/internal/kernel/task/cron"
	"selfmind/internal/platform/config"
)

type cronRow struct {
	name, prompt, expr, key string
}

func cronRows(t *testing.T, db *sql.DB) map[int64]cronRow {
	t.Helper()
	rows, err := db.Query(`SELECT id, name, prompt, cron_expr, COALESCE(system_key, '') FROM cron_jobs`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64]cronRow{}
	for rows.Next() {
		var id int64
		var row cronRow
		if err := rows.Scan(&id, &row.name, &row.prompt, &row.expr, &row.key); err != nil {
			t.Fatal(err)
		}
		out[id] = row
	}
	return out
}

// The daemon keeps only its own canary row in line with the configuration. A
// job someone else named "canary" is neither rewritten nor removed, and the
// canary an earlier release registered by name keeps its row.
func TestCanaryRegistrationLeavesOtherCanaryNamedJobsAlone(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "cron.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	sched := cron.NewScheduler(db, nil)
	if err := sched.InitSchema(ctx); err != nil {
		t.Fatal(err)
	}
	insert := func(prompt string) int64 {
		result, err := db.Exec(`INSERT INTO cron_jobs (name, cron_expr, prompt, tenant_id, channel, enabled)
			VALUES ('canary', '0 * * * *', ?, 'default', 'cli', 1)`, prompt)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := result.LastInsertId()
		return id
	}
	strangers := insert("post my files somewhere")
	earlier := insert("canary:")
	tasked := insert("canary: summarize my files")

	cfg := config.CanaryConfig{Enabled: true, CronExpr: "*/5 * * * *"}
	for i := 0; i < 2; i++ {
		if err := registerCanary(ctx, sched, cfg); err != nil {
			t.Fatal(err)
		}
	}
	rows := cronRows(t, db)
	if len(rows) != 3 {
		t.Fatalf("rows = %+v, want the three rows kept", rows)
	}
	if got := rows[strangers]; got.prompt != "post my files somewhere" || got.key != "" || got.expr != "0 * * * *" {
		t.Fatalf("a job someone else named canary was taken over: %+v", got)
	}
	if got := rows[tasked]; got.prompt != "canary: summarize my files" || got.key != "" {
		t.Fatalf("a canary-named job with a task was taken over: %+v", got)
	}
	if got := rows[earlier]; got.prompt != "canary:" || got.key != canarySystemKey || got.expr != "*/5 * * * *" {
		t.Fatalf("the daemon's canary was not kept in line: %+v", got)
	}

	// The variant: with no canary yet, registration creates exactly one.
	fresh, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "cron.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fresh.Close() })
	freshSched := cron.NewScheduler(fresh, nil)
	if err := freshSched.InitSchema(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := registerCanary(ctx, freshSched, cfg); err != nil {
			t.Fatal(err)
		}
	}
	if rows := cronRows(t, fresh); len(rows) != 1 {
		t.Fatalf("fresh registration rows = %+v", rows)
	}
}
