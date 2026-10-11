package cliapp

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/gateway/httpapi"
	"selfmind/internal/kernel/task/cron"
)

// scheduleReviewSection lists the reviewer's schedules that do not run, and
// the ones no one can be shown to own, each with its reason and how to remove
// it. Other people's schedules are only counted. Read-only.
func scheduleReviewSection(ctx context.Context, store *control.Store, identity *control.IdentityContext, dataDir string) string {
	path := filepath.Join(dataDir, "cron.db")
	if _, err := os.Stat(path); err != nil {
		return "(no schedules)\n"
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return fmt.Sprintf("(not checked: %v)\n", err)
	}
	defer db.Close()
	jobs, err := cron.NewScheduler(db, nil).ListJobs(ctx, identity.TenantID)
	if err != nil {
		return fmt.Sprintf("(not checked: %s)\n", oneLine(err.Error(), 160))
	}
	review, err := httpapi.ReviewSchedules(ctx, store, identity, jobs)
	if err != nil {
		return fmt.Sprintf("(not checked: %s)\n", oneLine(err.Error(), 160))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "jobs: %d yours, %d unclaimed, %d belong to other people (not shown)\n", review.Own, review.Unclaimed, review.Others)
	for _, stuck := range review.Stuck {
		state := "off"
		if stuck.Enabled {
			state = "on"
		}
		kind := "job"
		if stuck.Unclaimed {
			kind = "unclaimed job"
		}
		fmt.Fprintf(&sb, "warning: %s %d %q (%s) does not run: %s\n", kind, stuck.ID, oneLine(stuck.Name, 60), state, stuck.Reason)
		if stuck.Unclaimed && stuck.NamesNoAccount {
			fmt.Fprintf(&sb, "  if it is yours, see what it does and adopt it: selfmind cron adopt %d\n", stuck.ID)
		}
		fmt.Fprintf(&sb, "  remove it: selfmind send \"remove cron job %d\"\n", stuck.ID)
	}
	return sb.String()
}
