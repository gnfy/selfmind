package cliapp

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/control"
)

// selfmind maintenance migrate-task-references [--apply] [--limit N] [--data-dir DIR]
//
// The command is dry-run by default. It imports a historical work key only
// when its exact surface form occurs in the original user input for that run.
func (a *App) runMaintenanceMigrateTaskReferences(args []string) int {
	fs := flag.NewFlagSet("selfmind maintenance migrate-task-references", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	apply := fs.Bool("apply", false, "import exact legacy user-text evidence")
	limit := fs.Int("limit", 5000, "maximum historical work-key rows to inspect (1-100000)")
	dataDir := fs.String("data-dir", "", "control data directory (default: the configured storage data dir)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *limit < 1 || *limit > 100000 {
		fmt.Fprintln(a.stderr, "migrate-task-references: --limit must be between 1 and 100000")
		return 2
	}
	dir := strings.TrimSpace(*dataDir)
	if dir == "" {
		dir = a.gatewayDataDir()
	}
	store, err := control.OpenStore(dir)
	if err != nil {
		fmt.Fprintf(a.stderr, "migrate-task-references: cannot open control store: %v\n", err)
		return 1
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()
	result, err := store.MigrateLegacyTaskReferences(ctx, a.tenantID(), *limit, *apply)
	if err != nil {
		fmt.Fprintf(a.stderr, "migrate-task-references: %v\n", err)
		return 1
	}
	for _, finding := range result.Findings {
		label := "SKIP"
		if finding.Eligible {
			label = "IMPORT"
			if finding.Imported {
				label = "IMPORTED"
			}
		}
		fmt.Fprintf(a.stdout, "%s run=%s task=%s reference=%q reason=%s\n",
			label, finding.RunID, finding.TaskID, finding.Value, finding.Reason)
	}
	fmt.Fprintf(a.stdout, "Scan result: scanned %d, exact %d, inferred skipped %d, already imported %d, applied %d.\n",
		result.Scanned, result.Eligible, result.SkippedInferred, result.AlreadyImported, result.Applied)
	if !*apply && result.Eligible > result.AlreadyImported {
		fmt.Fprintln(a.stdout, "Re-run with --apply to import only the exact user-text evidence.")
	}
	return 0
}
