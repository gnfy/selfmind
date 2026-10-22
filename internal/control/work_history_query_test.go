package control

import (
	"context"
	"testing"
	"time"
)

func TestHistoryWindowUsesRunActivityAndStablePersonScopedPages(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner, _ := s.ResolveOrCreateAccount(ctx, DefaultTenantID, "cli", "history-owner", "Owner")
	other, _ := s.ResolveOrCreateAccount(ctx, DefaultTenantID, "cli", "history-other", "Other")
	start, _ := time.Parse(time.RFC3339, "2026-10-16T00:00:00+08:00")
	makeRun := func(person *IdentityContext, title string, when time.Time, workspace string) *Run {
		task, err := s.CreateTask(ctx, TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, Title: title, Channel: "cli"})
		if err != nil {
			t.Fatal(err)
		}
		run, err := s.StartRun(ctx, task, "cli", title)
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.db.Exec(`UPDATE runs SET started_at=?,workspace_id=? WHERE id=?`, when.Unix(), workspace, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	a := makeRun(owner, "review alpha", start.Add(time.Hour), "ws-a")
	b := makeRun(owner, "review beta", start.Add(time.Hour), "ws-a")
	_ = makeRun(owner, "review midnight excluded", start.Add(24*time.Hour), "ws-a")
	_ = makeRun(other, "review private", start.Add(time.Hour), "ws-a")
	_ = makeRun(owner, "review other workspace", start.Add(time.Hour), "ws-b")
	long := makeRun(owner, "review spanning midnight", start.Add(-time.Hour), "ws-a")
	if _, err := s.db.Exec(`INSERT INTO task_events(id,thread_id,run_id,type,payload_json,created_at) VALUES(?,?,?,?,?,?)`, "history-activity", long.TaskID, long.ID, "tool.started", "{}", start.Add(2*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if err := NewWorkTimeline(s).Archive(ctx, owner.TenantID, owner.PersonID, a.TaskID); err != nil {
		t.Fatal(err)
	}
	q := WorkHistoryQuery{Since: start, Until: start.Add(24 * time.Hour), WorkspaceID: "ws-a", Keyword: "review", Limit: 1}
	timeline := NewWorkTimeline(s)
	page, err := timeline.QueryHistory(ctx, owner.TenantID, owner.PersonID, q)
	if err != nil || len(page.Runs) != 1 || !page.HasMore || page.NextCursor == "" {
		t.Fatalf("first page=%+v err=%v", page, err)
	}
	seen := map[string]bool{page.Runs[0].ID: true}
	// A new, backdated insertion cannot enter an already-started page walk.
	inserted := makeRun(owner, "review inserted during paging", start.Add(30*time.Minute), "ws-a")
	for page.HasMore {
		q.Cursor = page.NextCursor
		page, err = timeline.QueryHistory(ctx, owner.TenantID, owner.PersonID, q)
		if err != nil {
			t.Fatal(err)
		}
		for _, run := range page.Runs {
			if seen[run.ID] {
				t.Fatal("duplicate Run across pages")
			}
			seen[run.ID] = true
		}
	}
	if len(seen) != 3 || !seen[a.ID] || !seen[b.ID] || !seen[long.ID] || seen[inserted.ID] {
		t.Fatalf("incorrect history window: %v", seen)
	}
	q.Keyword = "different query"
	if _, err = timeline.QueryHistory(ctx, owner.TenantID, owner.PersonID, q); err == nil {
		t.Fatal("cursor accepted different filters")
	}
	q.Keyword = "review"
	if _, err = timeline.QueryHistory(ctx, other.TenantID, other.PersonID, q); err == nil {
		t.Fatal("cursor accepted another person")
	}
}
