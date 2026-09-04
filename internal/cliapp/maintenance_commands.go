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
	if len(a.args) >= 3 && a.args[2] == "cleanup-person-partitions" {
		return true, a.runMaintenanceCleanupPersonPartitions(a.args[3:])
	}
	if len(a.args) >= 3 && a.args[2] == "prune-skill-candidate-refs" {
		return true, a.runMaintenancePruneSkillCandidateRefs(a.args[3:])
	}
	if len(a.args) >= 3 && a.args[2] == "memory-audit" {
		return true, a.runMaintenanceMemoryAudit(a.args[3:])
	}
	if len(a.args) >= 3 && a.args[2] == "memory-dedup" {
		return true, a.runMaintenanceMemoryDedup(a.args[3:])
	}
	if len(a.args) >= 3 && a.args[2] == "memory-archive-environment" {
		return true, a.runMaintenanceMemoryArchiveEnvironment(a.args[3:])
	}
	if len(a.args) >= 3 && a.args[2] == "task-audit" {
		return true, a.runMaintenanceTaskAudit(a.args[3:])
	}
	if len(a.args) >= 3 && a.args[2] == "restore-control" {
		return true, a.runMaintenanceRestoreControl(a.args[3:])
	}
	if len(a.args) >= 3 && a.args[2] == "reset-work-history" {
		return true, a.runMaintenanceResetWorkHistory(a.args[3:])
	}
	if len(a.args) < 3 || a.args[2] != "replay" {
		fmt.Fprintln(a.stderr, "usage: selfmind maintenance replay [--limit N] [--prompt-revision]")
		fmt.Fprintln(a.stderr, "       selfmind maintenance migrate-memory [--apply] [--data-dir DIR]")
		fmt.Fprintln(a.stderr, "       selfmind maintenance migrate-skills [--apply] [--root DIR] [--governance-grace 30d]")
		fmt.Fprintln(a.stderr, "       selfmind maintenance cleanup-person-partitions [--apply] [--root DIR --data-dir DIR]")
		fmt.Fprintln(a.stderr, "       selfmind maintenance prune-skill-candidate-refs [--apply] [--data-dir DIR]")
		fmt.Fprintln(a.stderr, "       selfmind maintenance memory-audit [--archive-confirmed] [--partition P] [--data-dir DIR]")
		fmt.Fprintln(a.stderr, "       selfmind maintenance memory-dedup [--apply] [--partition P] [--data-dir DIR]")
		fmt.Fprintln(a.stderr, "       selfmind maintenance memory-archive-environment [--apply] [--partition P] [--data-dir DIR]")
		fmt.Fprintln(a.stderr, "       selfmind maintenance task-audit [--apply] [--limit N] [--data-dir DIR]")
		fmt.Fprintln(a.stderr, "       selfmind maintenance migrate-task-references [--apply] [--limit N] [--data-dir DIR]")
		fmt.Fprintln(a.stderr, "       selfmind maintenance restore-control --backup PATH --yes [--data-dir DIR]")
		fmt.Fprintln(a.stderr, "       selfmind maintenance reset-work-history [--apply] [--data-dir DIR]")
		return true, 2
	}
	fs := flag.NewFlagSet("selfmind maintenance replay", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	limit := fs.Int("limit", 100, "maximum jobs to requeue")
	// Prompt-revision replay is operator-triggered by contract: requeueing
	// before the pinned revision is restored returns the work to the same
	// blocked state, while resetting attempts and overwriting last_error
	// destroys the failure ordering the maintenance health view reads. Folding
	// it into the default replay churned those rows on every invocation.
	promptRevision := fs.Bool("prompt-revision", false,
		"requeue work parked on a missing pinned prompt revision (only after restoring it)")
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

	if *promptRevision {
		requeued, replayErr := store.ReplayPromptRevisionMaintenanceJobs(ctx, a.tenantID(), *limit)
		if replayErr != nil {
			fmt.Fprintf(a.stderr, "maintenance replay: %v\n", replayErr)
			return true, 1
		}
		fmt.Fprintf(a.stdout, "Requeued %d prompt-revision-blocked maintenance job(s). The daemon will process them in the background.\n", requeued)
		if requeued == 0 {
			fmt.Fprintln(a.stdout, "Nothing was parked on a missing prompt revision.")
		}
		return true, 0
	}

	requeued, err := store.ReplayRetryLimitedMaintenanceJobs(ctx, a.tenantID(), *limit)
	if err != nil {
		fmt.Fprintf(a.stderr, "maintenance replay: %v\n", err)
		return true, 1
	}
	fmt.Fprintf(a.stdout, "Requeued %d retry-limited maintenance job(s). The daemon will process them in the background.\n", requeued)
	// Parked prompt-revision work is invisible to this scope, so name it rather
	// than leaving the operator to conclude the backlog is empty.
	if blocked, countErr := store.CountBlockedPromptRevisionMaintenanceJobs(ctx, a.tenantID()); countErr == nil && blocked > 0 {
		fmt.Fprintf(a.stdout, "%d job(s) are parked on a missing pinned prompt revision. Restore the revision, then run `selfmind maintenance replay --prompt-revision`.\n", blocked)
	}
	return true, 0
}
