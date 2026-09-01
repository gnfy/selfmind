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
// Read-only Task/Run continuity audit. Findings cover legacy resume edges the
// v7 backfill could not convert, illegal parent edges, ownerless pending
// approvals/clarifies, and task status projections that disagree with the
// derived reduction. --apply reconciles ONLY projection mismatches through
// the production reducer; every other finding stays human review.
func (a *App) runMaintenanceTaskAudit(args []string) int {
	fs := flag.NewFlagSet("selfmind maintenance task-audit", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	apply := fs.Bool("apply", false, "reconcile only projection-mismatch findings via the reducer")
	limit := fs.Int("limit", 200, "maximum findings/tasks to inspect (1-1000)")
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
	findings, err := store.AuditTaskRunContinuity(ctx, a.tenantID(), *limit)
	if err != nil {
		fmt.Fprintf(a.stderr, "task-audit: %v\n", err)
		return 1
	}
	if len(findings) == 0 {
		fmt.Fprintln(a.stdout, "No continuity findings.")
		return 0
	}
	safe, review, applied := 0, 0, 0
	for _, finding := range findings {
		label := "REVIEW"
		if finding.SafeFix {
			label = "RECONCILE"
			safe++
		} else {
			review++
		}
		fmt.Fprintf(a.stdout, "%s kind=%s task=%s run=%s %s\n",
			label, finding.Kind, valueOrDash(finding.TaskID), valueOrDash(finding.RunID), finding.Detail)
		if *apply && finding.SafeFix {
			if applyErr := store.ReconcileTaskProjection(ctx, a.tenantID(), finding.TaskID); applyErr != nil {
				fmt.Fprintf(a.stderr, "task-audit: apply %s: %v\n", finding.TaskID, applyErr)
				return 1
			}
			applied++
		}
	}
	fmt.Fprintf(a.stdout, "Scan result: reconcile %d, review %d, applied %d. Runs, edges, and memory were not changed.\n", safe, review, applied)
	if !*apply && safe > 0 {
		fmt.Fprintln(a.stdout, "Re-run with --apply to reconcile only the RECONCILE entries.")
	}
	return 0
}

func valueOrDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return strings.TrimSpace(value)
}
