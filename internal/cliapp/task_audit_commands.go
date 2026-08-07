package cliapp

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/control"
)

// selfmind maintenance task-audit [--apply] [--limit N] [--data-dir DIR]
//
// The command is dry-run by default. --apply only materializes missing blocker
// evidence when an inactive task and its newest finished run have the exact
// same blocker status. Conflicting histories remain human-review findings.
func (a *App) runMaintenanceTaskAudit(args []string) int {
	fs := flag.NewFlagSet("selfmind maintenance task-audit", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	apply := fs.Bool("apply", false, "backfill only unambiguous missing task blockers")
	limit := fs.Int("limit", 200, "maximum parked tasks to inspect (1-1000)")
	dataDir := fs.String("data-dir", "", "control data directory (default: the configured storage data dir)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *limit < 1 || *limit > 1000 {
		fmt.Fprintln(a.stderr, "task-audit: --limit must be between 1 and 1000")
		return 2
	}
	dir := strings.TrimSpace(*dataDir)
	if dir == "" {
		dir = a.gatewayDataDir()
	}
	store, err := control.OpenStore(dir)
	if err != nil {
		fmt.Fprintf(a.stderr, "task-audit: cannot open control store: %v\n", err)
		return 1
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(a.ctx, 20*time.Second)
	defer cancel()
	findings, err := store.AuditMissingTaskBlockers(ctx, a.tenantID(), *limit)
	if err != nil {
		fmt.Fprintf(a.stderr, "task-audit: %v\n", err)
		return 1
	}
	if len(findings) == 0 {
		fmt.Fprintln(a.stdout, "No missing task blocker evidence found.")
		return 0
	}
	safe, review, applied := 0, 0, 0
	for _, finding := range findings {
		label := "REVIEW"
		if finding.SafeToApply {
			label = "BACKFILL"
			safe++
		} else {
			review++
		}
		fmt.Fprintf(a.stdout, "%s task=%s status=%s latest_run=%s/%s kind=%s title=%q reason=%s\n",
			label, finding.TaskID, finding.TaskStatus, valueOrDash(finding.LatestRunID),
			valueOrDash(finding.LatestRunStatus), valueOrDash(finding.BlockerKind),
			truncateTaskAuditTitle(finding.Title), finding.Reason)
		if *apply && finding.SafeToApply {
			changed, applyErr := store.BackfillTaskBlocker(ctx, a.tenantID(), finding)
			if applyErr != nil {
				fmt.Fprintf(a.stderr, "task-audit: apply %s: %v\n", finding.TaskID, applyErr)
				return 1
			}
			if changed {
				applied++
			}
		}
	}
	fmt.Fprintf(a.stdout, "Scan result: safe %d, review %d, applied %d. Task and run statuses were not changed.\n", safe, review, applied)
	if !*apply && safe > 0 {
		fmt.Fprintln(a.stdout, "Re-run with --apply to backfill only the BACKFILL entries.")
	}
	return 0
}

func valueOrDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return strings.TrimSpace(value)
}

func truncateTaskAuditTitle(value string) string {
	runes := []rune(strings.Join(strings.Fields(value), " "))
	if len(runes) > 80 {
		return string(runes[:80]) + "..."
	}
	return string(runes)
}
