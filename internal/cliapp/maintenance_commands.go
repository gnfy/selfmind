package cliapp

import (
	"context"
	"flag"
	"fmt"
	"time"

	"selfmind/internal/control"
)

func (a *App) runMaintenanceCommandIfRequested() (bool, int) {
	if len(a.args) < 2 || a.args[1] != "maintenance" {
		return false, 0
	}
	if len(a.args) >= 3 && a.args[2] == "migrate-memory" {
		return true, a.runMaintenanceMigrateMemory(a.args[3:])
	}
	if len(a.args) >= 3 && a.args[2] == "migrate-skills" {
		return true, a.runMaintenanceMigrateSkills(a.args[3:])
	}
	if len(a.args) >= 3 && a.args[2] == "memory-audit" {
		return true, a.runMaintenanceMemoryAudit(a.args[3:])
	}
	if len(a.args) >= 3 && a.args[2] == "memory-dedup" {
		return true, a.runMaintenanceMemoryDedup(a.args[3:])
	}
	if len(a.args) >= 3 && a.args[2] == "task-audit" {
		return true, a.runMaintenanceTaskAudit(a.args[3:])
	}
	if len(a.args) >= 3 && a.args[2] == "migrate-task-references" {
		return true, a.runMaintenanceMigrateTaskReferences(a.args[3:])
	}
	if len(a.args) >= 3 && a.args[2] == "restore-control" {
		return true, a.runMaintenanceRestoreControl(a.args[3:])
	}
	if len(a.args) < 3 || a.args[2] != "replay" {
		fmt.Fprintln(a.stderr, "usage: selfmind maintenance replay [--limit N]")
		fmt.Fprintln(a.stderr, "       selfmind maintenance migrate-memory [--apply] [--data-dir DIR]")
		fmt.Fprintln(a.stderr, "       selfmind maintenance migrate-skills [--apply] [--root DIR] [--governance-grace 30d]")
		fmt.Fprintln(a.stderr, "       selfmind maintenance memory-audit [--archive-confirmed] [--partition P] [--data-dir DIR]")
		fmt.Fprintln(a.stderr, "       selfmind maintenance memory-dedup [--apply] [--partition P] [--data-dir DIR]")
		fmt.Fprintln(a.stderr, "       selfmind maintenance task-audit [--apply] [--limit N] [--data-dir DIR]")
		fmt.Fprintln(a.stderr, "       selfmind maintenance migrate-task-references [--apply] [--limit N] [--data-dir DIR]")
		fmt.Fprintln(a.stderr, "       selfmind maintenance restore-control --backup PATH --yes [--data-dir DIR]")
		return true, 2
	}
	fs := flag.NewFlagSet("selfmind maintenance replay", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	limit := fs.Int("limit", 100, "maximum retry-limited jobs to requeue")
	if err := fs.Parse(a.args[3:]); err != nil {
		return true, 2
	}
	if *limit <= 0 || *limit > 500 {
		fmt.Fprintln(a.stderr, "maintenance replay: --limit must be between 1 and 500")
		return true, 2
	}

	store, err := control.OpenStore(a.gatewayDataDir())
	if err != nil {
		fmt.Fprintf(a.stderr, "maintenance replay: cannot open control store: %v\n", err)
		return true, 1
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()
	requeued, err := store.ReplayRetryLimitedMaintenanceJobs(ctx, a.tenantID(), *limit)
	if err != nil {
		fmt.Fprintf(a.stderr, "maintenance replay: %v\n", err)
		return true, 1
	}
	fmt.Fprintf(a.stdout, "Requeued %d maintenance jobs. The daemon will process them in the background.\n", requeued)
	return true, 0
}
